package multiapply

// T89: captcha postpone policy — a captcha challenge parks the vacancy
// (retry_at = now + retry_after) instead of skipping it, the Nth hit of a
// pass pauses the apply phase for the cooldown (AI work continues, the
// ready letters apply first after the resume) and due postpones are
// re-examined with fresh vacancy data (archived → skip, alive → apply).
// mode="skip" keeps the pre-T89 day-breaker behavior byte-for-byte.

import (
	"context"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// captchaAllTrip returns an applyErr that captcha-challenges every POST.
func captchaAllTrip() func(int, int64, string) error {
	return func(int, int64, string) error {
		return &respErr{redirect: "/captcha/image?backurl=x"}
	}
}

// featureAt clones the harness feature with a caller-controlled clock —
// the postpone re-exam needs the pass clock to cross retry_at.
func (h *harness) featureAt(t *testing.T, clock func() time.Time) *Feature {
	t.Helper()
	chainFor := func(string) (captcha.Chain, bool) { return h.chain, true }
	if h.noChain {
		chainFor = nil
	}
	var runner ai.Runner
	if !h.noRunner {
		runner = h.runner
	}
	return New(Options{
		Config:   h.cfg,
		DB:       h.db,
		APIFor:   func(string) (hhapi.HHAPI, error) { return h.api, nil },
		ChainFor: chainFor,
		Runner:   runner,
		ProfileFor: func(context.Context, string) (profile.Profile, error) {
			return h.profile, nil
		},
		Notifier: h.notify,
		Now:      clock,
		Log:      quietLogger(),
		Console:  h.console,
	})
}

// parkedRows reads the postpone registry (everything parked so far: the
// due-bound sits an hour ahead of the wall clock).
func parkedRows(t *testing.T, db *store.DB, accountID string) []store.CaptchaPostponed {
	t.Helper()
	rows, err := db.CaptchaPostponedDue(t.Context(), accountID, "hash1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CaptchaPostponedDue: %v", err)
	}
	return rows
}

// The first captcha challenge parks the vacancy (postpone registry row with
// retry_at = now + retry_after) instead of the captcha_exhausted skip; the
// ledger keeps no failed row and the wake task lands in the queue.
func TestPostponeParksOnCaptcha(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777070, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()

	h.run(t)

	rows := parkedRows(t, h.db, "acc1")
	if len(rows) != 1 {
		t.Fatalf("postponed rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.VacancyID != 777070 || row.ResumeHash != "hash1" || row.RunID != "run1" {
		t.Fatalf("row = %+v", row)
	}
	if row.RetryCount != 1 {
		t.Fatalf("retry count = %d, want 1", row.RetryCount)
	}
	if want := now.Add(2 * time.Hour); !row.RetryAt.Equal(want) {
		t.Fatalf("retry_at = %v, want %v", row.RetryAt, want)
	}
	if m := mustMetrics(t, h.db)["apply.skip.captcha_postponed"]; m != 1 {
		t.Fatalf("apply.skip.captcha_postponed = %d, want 1", m)
	}
	// The parking wake rides the T71 queue at retry_at.
	if w, ok := h.q.NextDeadline("acc1"); !ok || !w.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("queue next deadline = %v ok=%v, want retry_at", w, ok)
	}
	open, _ := h.db.ReviewItemsOpen(t.Context(), "acc1", 10)
	for _, it := range open {
		if it.Kind == "captcha_exhausted" {
			t.Fatalf("postpone must not park a captcha_exhausted review: %+v", open)
		}
	}
	if apps, err := h.db.ApplicationsListByAccountAndVacancies(t.Context(), "acc1", []int64{777070}); err != nil || len(apps) != 0 {
		t.Fatalf("postpone must not write a ledger row: %v %+v", err, apps)
	}
}

// The limit-th captcha of a pass pauses the apply phase: the remaining
// candidate's letter is prepared during the pause (AI work) and applies
// after the resume; the captcha victims stay parked.
func TestPostponeCooldownAfterLimit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.Captcha.Limit = 3
		c.Features.MultiApply.Captcha.Cooldown = "80ms"
	})
	buf := h.withConsole(t, nil)
	h.noChain = true
	h.searchItems = []string{
		vacItem(777071, "Go Dev A", 9001, "Corp LLC", ""),
		vacItem(777072, "Go Dev B", 9002, "Other LLC", ""),
		vacItem(777073, "Go Dev C", 9003, "Third LLC", ""),
		vacItem(777074, "Go Dev D", 9004, "Fourth LLC", ""),
	}
	for emp := int64(9001); emp <= 9004; emp++ {
		h.ratings[emp] = hhRating(4.5, 4.7, 100)
	}
	h.applyErr = func(_ int, vacancy int64, _ string) error {
		if vacancy <= 777073 {
			return &respErr{redirect: "/captcha/image?backurl=x"}
		}
		return nil
	}

	h.run(t)

	rows := parkedRows(t, h.db, "acc1")
	if len(rows) != 3 {
		t.Fatalf("postponed rows = %d, want the 3 captcha victims", len(rows))
	}
	if m := mustMetrics(t, h.db)["apply.ok"]; m != 1 {
		t.Fatalf("apply.ok = %d, want the ready candidate applied after the resume", m)
	}
	out := buf.String()
	if !strings.Contains(out, "· Капча-лимит: пауза откликов до") {
		t.Fatalf("cooldown line missing: %q", out)
	}
	if !strings.Contains(out, "· Пауза окончена: отклики возобновлены") {
		t.Fatalf("resume line missing: %q", out)
	}
	if strings.Contains(out, "Капча-брейкер") {
		t.Fatalf("postpone must not trip the day breaker: %q", out)
	}
	if h.notify.count(webhook.EvCaptchaExhausted) != 0 {
		t.Fatalf("captcha_exhausted webhook fired in postpone mode")
	}
}

// The re-exam refreshes the vacancy data: an archived vacancy settles as
// the vacancy_archived skip and leaves the registry.
func TestPostponeReexamArchived(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777075, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	clock := now
	h.runAt(t, func() time.Time { return clock })

	clock = now.Add(2 * time.Hour).Add(time.Minute) // retry_at passed
	h.cardByVac[777075] = `{"id":777075,"name":"Go Dev A","archived":true,
		"company":{"id":9001,"name":"Corp LLC"}}`
	h.applyErr = nil
	h.runAt(t, func() time.Time { return clock })

	if m := mustMetricsAt(t, h.db, clock)["apply.skip.vacancy_archived"]; m != 1 {
		t.Fatalf("apply.skip.vacancy_archived = %d, want 1", m)
	}
	if rows := parkedRows(t, h.db, "acc1"); len(rows) != 0 {
		t.Fatalf("archived vacancy still parked: %+v", rows)
	}
	if len(h.applyCalls) != 1 { // only the first-pass captcha POST
		t.Fatalf("apply calls = %v, archived vacancy must not re-POST", h.applyCalls)
	}
}

// A live vacancy re-attacked at retry_at applies normally.
func TestPostponeReexamAliveApplies(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777076, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	clock := now
	h.runAt(t, func() time.Time { return clock })

	clock = now.Add(2 * time.Hour).Add(time.Minute)
	h.applyErr = nil
	h.runAt(t, func() time.Time { return clock })

	if m := mustMetricsAt(t, h.db, clock)["apply.ok"]; m != 1 {
		t.Fatalf("apply.ok = %d, want the due postpone applied", m)
	}
	if m := mustMetricsAt(t, h.db, clock)["apply.postponed_retry"]; m != 1 {
		t.Fatalf("apply.postponed_retry = %d, want 1", m)
	}
	if rows := parkedRows(t, h.db, "acc1"); len(rows) != 0 {
		t.Fatalf("applied vacancy still parked: %+v", rows)
	}
}

// A captcha during the re-exam re-parks the vacancy; exhausting max_retries
// settles it as the captcha_exhausted skip and drops the row.
func TestPostponeMaxRetriesExhausted(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.Captcha.MaxRetries = 2
		c.Features.MultiApply.Captcha.RetryAfter = "1ms"
	})
	h.noChain = true
	h.searchItems = []string{vacItem(777077, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	clock := now
	h.runAt(t, func() time.Time { return clock }) // pass 1: park (count 1)

	clock = clock.Add(2 * time.Second)
	h.runAt(t, func() time.Time { return clock }) // pass 2: captcha again → count 2 >= max_retries

	if rows := parkedRows(t, h.db, "acc1"); len(rows) != 0 {
		t.Fatalf("exhausted vacancy still parked: %+v", rows)
	}
	m := mustMetricsAt(t, h.db, clock)
	if m["apply.skip.captcha_exhausted"] != 1 {
		t.Fatalf("apply.skip.captcha_exhausted = %d, want 1 (in %+v)", m["apply.skip.captcha_exhausted"], m)
	}
}

// A future retry_at is not re-attacked by the re-exam: the pass leaves the
// row alone and never fetches the card for it.
func TestPostponeNotDueUntouched(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777078, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	h.run(t) // park at frozen `now`

	h.cardReqs = map[int64]int{}
	h.applyErr = nil
	h.run(t) // same instant: retry_at still 2h away

	if h.cardReqs[777078] != 0 {
		t.Fatalf("card fetched %d times for a not-yet-due postpone", h.cardReqs[777078])
	}
	if rows := parkedRows(t, h.db, "acc1"); len(rows) != 1 {
		t.Fatalf("postponed rows = %d, want the row kept", len(rows))
	}
}

// The postponed vacancy never blocks a future apply: the fresh pool
// re-attacks it before retry_at and the row settles once applied.
func TestPostponeDedupNotBlocking(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777079, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	clock := now
	h.runAt(t, func() time.Time { return clock }) // park

	h.applyErr = nil
	clock = clock.Add(time.Minute)
	h.runAt(t, func() time.Time { return clock }) // fresh pool re-applies before retry_at

	if m := mustMetricsAt(t, h.db, clock)["apply.ok"]; m != 1 {
		t.Fatalf("apply.ok = %d, want the parked vacancy applied from the fresh pool", m)
	}
	// A later due re-exam sees the applied ledger row and drops the leftover
	// registry row without a POST.
	clock = clock.Add(3 * time.Hour)
	callsBefore := len(h.applyCalls)
	h.runAt(t, func() time.Time { return clock })
	if len(h.applyCalls) != callsBefore {
		t.Fatalf("re-exam re-POSTed an applied vacancy: %v", h.applyCalls)
	}
	if rows := parkedRows(t, h.db, "acc1"); len(rows) != 0 {
		t.Fatalf("registry row survived an applied outcome: %+v", rows)
	}
}

// The popup's responseImpossible verdict (vacancy closed) settles the
// re-exam as vacancy_archived too.
func TestPostponeResponseImpossibleSkips(t *testing.T) {
	h := newHarness(t, nil)
	h.noChain = true
	h.searchItems = []string{vacItem(777080, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	clock := now
	h.runAt(t, func() time.Time { return clock })

	clock = now.Add(2 * time.Hour).Add(time.Minute)
	h.popupByVac[777080] = `{"type":"POPUP","responseImpossible":true,"letterRequired":false,"test":{"hasTests":false},"chatWritePossibility":"ENABLED"}`
	h.applyErr = nil
	h.runAt(t, func() time.Time { return clock })

	if m := mustMetricsAt(t, h.db, clock)["apply.skip.vacancy_archived"]; m != 1 {
		t.Fatalf("apply.skip.vacancy_archived = %d, want 1", m)
	}
	if rows := parkedRows(t, h.db, "acc1"); len(rows) != 0 {
		t.Fatalf("closed vacancy still parked: %+v", rows)
	}
}

// The postpone park renders its console line with the retry moment.
func TestConsolePostponeLines(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "interesting,aggregate" })
	h.noChain = true
	h.searchItems = []string{vacItem(777081, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()

	h.run(t)

	if !strings.Contains(buf.String(), "· Отложено (капча): «Go Dev A» — Corp LLC · повтор после 24.09 12:00") {
		t.Fatalf("park line missing: %q", buf.String())
	}
}

// The re-exam archived skip renders the Russian label.
func TestConsolePostponeArchivedLabel(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "interesting,aggregate" })
	h.noChain = true
	h.searchItems = []string{vacItem(777084, "Go Dev A", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = captchaAllTrip()
	clock := now
	h.runAt(t, func() time.Time { return clock })

	clock = now.Add(2 * time.Hour).Add(time.Minute)
	h.cardByVac[777084] = `{"id":777084,"name":"Go Dev A","archived":true,"company":{"id":9001,"name":"Corp LLC"}}`
	h.applyErr = nil
	h.runAt(t, func() time.Time { return clock })

	if !strings.Contains(buf.String(), "· Пропущен: «Go Dev A» — Corp LLC: вакансия архивна") {
		t.Fatalf("archived skip label missing: %q", buf.String())
	}
}

// runAt runs the profile with a caller-controlled clock.
func (h *harness) runAt(t *testing.T, clock func() time.Time) {
	t.Helper()
	f := h.featureAt(t, clock)
	if h.q == nil {
		h.q = queue.New()
		h.reg = scheduler.NewRegistry()
	}
	if err := f.RegisterTasks(t.Context(), h.q, h.reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if err := f.RunProfile(t.Context(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
}

func mustMetricsAt(t *testing.T, db *store.DB, at time.Time) map[string]int {
	t.Helper()
	m, err := db.MetricsSnapshot(t.Context(), featurekit.DayUTC(at))
	if err != nil {
		t.Fatalf("metrics snapshot: %v", err)
	}
	return m
}
