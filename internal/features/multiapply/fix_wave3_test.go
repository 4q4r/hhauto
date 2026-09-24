package multiapply

// Wave 3 review fix tests (fix/t10-wave3): cascade order, dry-run dedup,
// captcha circuit breaker, double-ambiguous symmetry.

import (
	"errors"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// FIX 1: F7 (quotas) must run BEFORE ApplyPopup (F6.5) — cheap DB checks
// before an expensive hh request.
func TestQuotaExhaustedSkipsPopup(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.MaxPerDay = 1
		c.Runs["run1"] = r
	})
	// One application recorded today already → the run quota is exhausted
	// before the first vacancy is processed.
	mustCreateApplication(t, h.db, featurekit.DedupKey("acc1", "hash1", 42), "applied")
	h.searchItems = []string{vacItem(777040, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if h.popupReqs[777040] != 0 {
		t.Fatalf("popup called despite exhausted quota (F7 must precede F6.5)")
	}
	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.stop.run_quota"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

// FIX 2: a dry_run survey row must never burn the dedup key: the live run
// applies to the same vacancy and upgrades the row instead of duplicating.
func TestDryRunThenLiveRunApplies(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.General.DryRun = true })
	h.searchItems = []string{vacItem(777041, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777041] = popupWith(true, false, "ENABLED")

	h.run(t) // dry
	if len(h.applyCalls) != 0 {
		t.Fatalf("dry run applied: %v", h.applyCalls)
	}
	app := mustApplication(t, h.db, "acc1", 777041)
	if app.Status != "dry_run" {
		t.Fatalf("dry-run row status = %q", app.Status)
	}

	// Live run for the same vacancy: the dry_run row must not block it.
	h.cfg.General.DryRun = false
	h.run(t)

	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777041 {
		t.Fatalf("live applies = %v, want [777041]", h.applyCalls)
	}
	app = mustApplication(t, h.db, "acc1", 777041)
	if app.Status != "applied" {
		t.Fatalf("upgraded row status = %q, want applied", app.Status)
	}
	if app.AppliedAt == nil {
		t.Fatalf("upgraded row must carry applied_at")
	}
	// The upgraded row counts toward today's quota (status applied).
	if n, _ := h.db.ApplicationsCountToday(t.Context(), "acc1", "run1", featurekit.DayUTC(now)); n != 1 {
		t.Fatalf("count today = %d, want 1", n)
	}
	if mustMetrics(t, h.db)["apply.ok"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

// FIX 3: captcha circuit breaker — after the first failed Solve the flag is
// set, the webhook fires, and further vacancies skip without Solve calls.
func TestCaptchaBreakerTripsOnSolveFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.chain.err = errors.New("all providers failed")
	h.searchItems = []string{
		vacItem(777042, "Go Dev A", 9001, "Corp LLC", ""),
		vacItem(777043, "Go Dev B", 9002, "Other LLC", ""),
	}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.ratings[9002] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(int, int64, string) error {
		return &respErr{redirect: "/captcha/image?backurl=x"}
	}

	h.run(t)

	if len(h.applyCalls) != 2 {
		t.Fatalf("apply calls = %v, want both attempted once", h.applyCalls)
	}
	if h.chain.solves != 1 {
		t.Fatalf("chain solves = %d, want 1 (breaker skips the rest)", h.chain.solves)
	}
	if !h.notify.has(webhook.EvCaptchaExhausted) {
		t.Fatalf("no %s webhook", webhook.EvCaptchaExhausted)
	}
	if n := h.notify.count(webhook.EvCaptchaExhausted); n != 1 {
		t.Fatalf("EvCaptchaExhausted emitted %d times, want 1", n)
	}
	open, _ := h.db.ReviewItemsOpen(t.Context(), "acc1", 10)
	if len(open) != 1 || open[0].Kind != "captcha_exhausted" {
		t.Fatalf("review items = %+v, want one captcha_exhausted", open)
	}
	if mustMetrics(t, h.db)["apply.skip.captcha_exhausted"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	if v, ok, _ := h.db.FeatureStateGet(t.Context(), "multiapply", "acc1", captchaBreakerKey("acc1", featurekit.DayUTC(now))); !ok || v != "1" {
		t.Fatalf("breaker flag = %q ok=%v, want set", v, ok)
	}
}

// FIX 3b: no configured chain trips the breaker too.
func TestCaptchaBreakerTripsWithoutChain(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777044, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(int, int64, string) error {
		return &respErr{redirect: "/captcha/image?backurl=x"}
	}

	h.run(t)

	if len(h.applyCalls) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(h.applyCalls))
	}
	if !h.notify.has(webhook.EvCaptchaExhausted) {
		t.Fatalf("no breaker webhook without chain")
	}
	if n := h.notify.count(webhook.EvCaptchaExhausted); n != 1 {
		t.Fatalf("EvCaptchaExhausted emitted %d times, want 1", n)
	}
	if mustMetrics(t, h.db)["apply.error"] != 1 {
		t.Fatalf("metrics = %v, want the challenge vacancy recorded as error", mustMetrics(t, h.db))
	}
	if v, ok, _ := h.db.FeatureStateGet(t.Context(), "multiapply", "acc1", captchaBreakerKey("acc1", featurekit.DayUTC(now))); !ok || v != "1" {
		t.Fatalf("breaker flag = %q ok=%v, want set", v, ok)
	}
}

// FIX 7: a second AmbiguousError inside the sanctioned retry is handled
// symmetrically — reconcile / review, never a silent "failed".
func TestAmbiguousRetryAmbiguousReconcileFound(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{vacItem(777045, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(int, int64, string) error {
		return &respErr{status: 502, body: "bad gateway"} // both attempts ambiguous
	}
	// 1st reconcile: empty (retry sanctioned); 2nd reconcile: found.
	h.negScript = []negAct{
		{pages: nil},
		{pages: []hhapi.Negotiation{{ID: "n1", VacancyID: 777045, State: "ACTIVE"}}},
	}

	h.run(t)

	if len(h.applyCalls) != 2 {
		t.Fatalf("apply calls = %d, want 2 (no third)", len(h.applyCalls))
	}
	app := mustApplication(t, h.db, "acc1", 777045)
	if app.Status != "applied" {
		t.Fatalf("status = %q, want applied via second reconcile", app.Status)
	}
	if mustMetrics(t, h.db)["apply.reconciled"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	if mustMetrics(t, h.db)["apply.error"] != 0 {
		t.Fatalf("second ambiguity must not be recorded as plain error: %v", mustMetrics(t, h.db))
	}
}

func TestAmbiguousRetryAmbiguousUnreadable(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{vacItem(777046, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(int, int64, string) error {
		return &respErr{status: 502, body: "bad gateway"}
	}
	// 1st reconcile: readable but empty (sanctions the retry);
	// 2nd reconcile: unreadable → park for a human.
	h.negScript = []negAct{
		{pages: nil},
		{fail: true},
	}

	h.run(t)

	if len(h.applyCalls) != 2 {
		t.Fatalf("apply calls = %d, want 2", len(h.applyCalls))
	}
	open, _ := h.db.ReviewItemsOpen(t.Context(), "acc1", 10)
	if len(open) != 1 || open[0].Kind != "apply_ambiguous" {
		t.Fatalf("review items = %+v, want apply_ambiguous", open)
	}
	claim, _ := featurekit.BeginIntent(t.Context(), h.db, "apply:acc1:777046", "{}")
	if claim.Step != featurekit.IntentSkipPending {
		t.Fatalf("intent step = %v, want pending (parked for human)", claim.Step)
	}
	if mustMetrics(t, h.db)["apply.ambiguous_review"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

var _ = store.Application{} // keep store import if assertions evolve
