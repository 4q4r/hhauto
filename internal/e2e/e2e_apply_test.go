package e2e

// E2E-1 apply-happy-path and E2E-2 cascade-skip: the full multiapply
// journey through cli wiring + engine.Run against the fake hh. Every wait
// is a store/fake end-state poll; nothing is time-based.

import (
	"context"
	"strings"
	"testing"
	"time"

	"hhauto/internal/audit"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/store"
)

// TestE2E1ApplyHappyPath: the clean vacancy passes the whole cascade, the
// letter comes from the real Runner over the fake OpenAI backend, the apply
// lands as 201 with the XSRF header, the application is recorded as applied,
// the intent is done, apply.ok is counted. A second full engine run on the
// same store dedups: zero new hh mutations.
func TestE2E1ApplyHappyPath(t *testing.T) {
	env := newEnv(t, nil)

	// --- run #1: discovery → cascade → applied ---
	cancel, done := env.runEngine()
	// Tail markers: apply.error==1 means the LAST vacancy of the batch
	// (captcha, 105) was processed; apply.ok==2 means both cascade-passing
	// vacancies (101 clean, 104 dup-unseeded) landed. Stopping the engine
	// only after the batch settles keeps the graceful drain out of the way.
	waitUntil(t, 30*time.Second, "apply batch fully settled", func() bool {
		app := env.application(vacClean)
		return app != nil && app.Status == "applied" &&
			env.hh.applyTryCount(vacClean) == 1 &&
			env.metricValue("apply.ok") == 2 &&
			env.metricValue("apply.error") == 1
	})
	env.stopEngine(cancel, done)

	app := env.application(vacClean)
	if app.HhResumeID != resumeID || app.RunID != runID || app.AccountID != acctID {
		t.Fatalf("applied row identity: %+v", app)
	}
	if app.AppliedAt == nil {
		t.Fatal("applied row has no applied_at")
	}
	if app.Letter == nil || *app.Letter != e2eLetter {
		t.Fatalf("applied letter = %v, want the fake-AI cover letter", app.Letter)
	}
	// Intent done: re-claiming the apply key must demand reconciliation.
	if step := env.intentStep("apply:" + acctID + ":101"); step != featurekit.IntentReconcileFirst {
		t.Fatalf("apply intent step = %v, want done (IntentReconcileFirst)", step)
	}
	// Fixture: 101 (clean) and 104 (dup, unseeded here) both pass the
	// cascade, 102/103/105 skip — so exactly two ok-applies land.
	if got := env.metricValue("apply.ok"); got != 2 {
		t.Fatalf("apply.ok = %d, want 2", got)
	}
	posts := env.hh.applyPostsSnapshot()
	// Three attempts on the wire: 101 ok, 104 ok, 105 captcha challenge.
	if len(posts) != 3 {
		t.Fatalf("apply posts = %d, want 3 (%+v)", len(posts), posts)
	}
	perVac := map[int64]int{}
	for _, p := range posts {
		perVac[p.VacancyID]++
		if p.XSRF != xsrfToken {
			t.Fatalf("apply POST for vacancy %d carries xsrf %q, want %q", p.VacancyID, p.XSRF, xsrfToken)
		}
		if p.ResumeHash != resumeID {
			t.Fatalf("apply POST for vacancy %d carries resume %q", p.VacancyID, p.ResumeHash)
		}
	}
	for _, v := range []int64{vacClean, vacDup, vacCaptcha} {
		if perVac[v] != 1 {
			t.Fatalf("vacancy %d apply attempts = %d, want 1", v, perVac[v])
		}
	}
	for _, p := range posts {
		if p.VacancyID == vacClean && p.Letter != e2eLetter {
			t.Fatalf("wire letter for 101 = %q", p.Letter)
		}
	}

	// --- run #2: full engine restart on the same store → dedup ---
	appliedBefore, err := func() (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return env.db.ApplicationsCountToday(ctx, acctID, runID, audit.Day())
	}()
	if err != nil {
		t.Fatal(err)
	}
	cancel2, done2 := env.runEngine()
	waitUntil(t, 30*time.Second, "second run re-skips all vacancies", func() bool {
		// 101 + 104 (applied rows) and 105 (failed row is live for
		// ApplicationsGetByDedupKeyLive) all land in already_applied.
		return env.metricValue("apply.skip.already_applied") == 3
	})
	env.stopEngine(cancel2, done2)

	if posts := env.hh.applyPostsSnapshot(); len(posts) != 3 {
		t.Fatalf("second run mutated hh: %d apply posts, want the original 3", len(posts))
	}
	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()
	appliedAfter, err := env.db.ApplicationsCountToday(ctx, acctID, runID, audit.Day())
	if err != nil {
		t.Fatal(err)
	}
	if appliedAfter != appliedBefore {
		t.Fatalf("second run created new applications: %d → %d", appliedBefore, appliedAfter)
	}
	if got := env.hh.applyTryCount(vacCaptcha); got != 1 {
		t.Fatalf("captcha vacancy apply attempts = %d, want 1 (no repeat)", got)
	}
}

// TestE2E2CascadeSkips: the four non-clean vacancies hit their own skip
// reasons — exclude word (F2), employer rating (F4), dedup against a live
// application row (F5 via ApplicationsGetByDedupKeyLive) and the captcha
// challenge on apply (no provider chain → breaker + failed record).
func TestE2E2CascadeSkips(t *testing.T) {
	env := newEnv(t, nil)

	// Seed the live (non-dry-run) duplicate: F5 must find it through
	// ApplicationsGetByDedupKeyLive and skip before any hh call.
	dupKey := featurekit.DedupKey(acctID, resumeID, vacDup)
	now := time.Now()
	seeded := &store.Application{
		AccountID: acctID, RunID: runID, HhResumeID: resumeID,
		VacancyID: vacDup, EmployerID: employerGood,
		DedupKey: dupKey, Status: "applied", AppliedAt: &now,
	}
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 5*time.Second)
	if err := env.db.ApplicationsCreate(seedCtx, seeded); err != nil {
		cancelSeed()
		t.Fatalf("seed duplicate application: %v", err)
	}
	cancelSeed()
	seededID := seeded.ID

	cancel, done := env.runEngine()
	waitUntil(t, 30*time.Second, "all four skip reasons counted", func() bool {
		return env.metricValue("apply.skip.exclude_words") == 1 &&
			env.metricValue("apply.skip.rating_low") == 1 &&
			env.metricValue("apply.skip.already_applied") == 1 &&
			env.metricValue("apply.error") == 1 &&
			env.metricValue("apply.ok") == 1
	})
	env.stopEngine(cancel, done)

	// F5: the seeded live row is the dedup gate — same row, untouched.
	liveCtx, cancelLive := context.WithTimeout(context.Background(), 5*time.Second)
	live, err := env.db.ApplicationsGetByDedupKeyLive(liveCtx, dupKey)
	cancelLive()
	if err != nil || live == nil {
		t.Fatalf("ApplicationsGetByDedupKeyLive(%v) = (%v, %v), want the seeded row", dupKey, live, err)
	}
	if live.ID != seededID || live.Status != "applied" {
		t.Fatalf("live dup row = %+v, want seeded id=%d applied", live, seededID)
	}

	// Skipped-before-popup vacancies never create application rows.
	if app := env.application(vacExclude); app != nil {
		t.Fatalf("exclude-word vacancy recorded an application: %+v", app)
	}
	if app := env.application(vacLowRating); app != nil {
		t.Fatalf("low-rating vacancy recorded an application: %+v", app)
	}
	// The captcha vacancy ends as a recorded failure, never repeated.
	capApp := env.application(vacCaptcha)
	if capApp == nil || capApp.Status != "failed" {
		t.Fatalf("captcha application = %+v, want status failed", capApp)
	}
	if capApp.FailReason == nil || !strings.Contains(strings.ToLower(*capApp.FailReason), "captcha") {
		t.Fatalf("captcha failure reason = %v, want a captcha marker", capApp.FailReason)
	}
	if got := env.hh.applyTryCount(vacCaptcha); got != 1 {
		t.Fatalf("captcha apply attempts = %d, want exactly 1 (breaker, no retry)", got)
	}

	// hh wire: only the clean vacancy was applied; the dup never reached
	// the popup/apply surface; nothing left chats.
	if got := env.hh.applyTryCount(vacDup); got != 0 {
		t.Fatalf("dup vacancy apply attempts = %d, want 0", got)
	}
	if got := env.hh.applyTryCount(vacExclude); got != 0 {
		t.Fatalf("exclude vacancy apply attempts = %d, want 0", got)
	}
	if got := env.hh.applyTryCount(vacLowRating); got != 0 {
		t.Fatalf("low-rating vacancy apply attempts = %d, want 0", got)
	}
	_, _, leaves, negotiationAppends := env.hh.stats()
	if leaves != 0 {
		t.Fatalf("unexpected chat leaves: %d", leaves)
	}
	if negotiationAppends != 1 {
		t.Fatalf("negotiations created = %d, want 1 (only the clean apply)", negotiationAppends)
	}
	// The captcha breaker is latched for the day.
	if got := env.featureState("multiapply", "captcha:exhausted:"+acctID+":"+audit.Day()); got != "1" {
		t.Fatalf("captcha breaker flag = %q, want 1", got)
	}
}
