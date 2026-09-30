package multiapply

import (
	"context"
	"net/http"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// T77 bug 1: a vacancy whose apply popup advertises required tests must be
// skipped (test_required) BEFORE the apply POST — the live daemon logged
// three 400 {"error":"test-required"} responses because popupGate parsed
// HasTests but never consulted it.
func TestPopupTestRequiredSkipsBeforeApply(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{vacItem(777031, "QA Engineer", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	// Live envelope shape (testdata/live/popup.json): the test block lives
	// under responseStatus.
	h.popupByVac[777031] = `{"type":"POPUP","responseStatus":{"test":{"hasTests":true}},"letterRequired":false,"test":{"hasTests":true},"acceptAutoResponse":true,"chatWritePossibility":"ENABLED"}`

	h.run(t)
	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (test-required vacancy must never POST)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1 (metrics %v)", m, mustMetrics(t, h.db))
	}
}

// T77 bug 2: a client-side 400/422 on apply is terminal for the pass — a
// second run profile sharing the vacancy must not re-POST it. Live: two
// consecutive test-required errors for one vacancy in one daemon cycle.
func TestValidationFailureTerminalWithinPass(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Runs["run2"] = config.Run{
			Enabled: true, Account: "acc1", ResumeID: "hash1",
			Query: "go", MaxPerDay: 10,
		}
	})
	h.searchItems = []string{vacItem(777032, "Senior Go Developer", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777032] = popupWith(false, false, "ENABLED") // popup: no tests — hh disagrees at POST time
	h.applyErr = func(_ int, _ int64, _ string) error {
		return &respErr{status: http.StatusBadRequest, body: `{"error":"test-required"}`}
	}

	f := h.feature(t)
	h.q, h.reg = queue.New(), scheduler.NewRegistry()
	if err := f.RegisterTasks(context.Background(), h.q, h.reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile run1: %v", err)
	}
	if err := f.RunProfile(context.Background(), "run2"); err != nil {
		t.Fatalf("RunProfile run2: %v", err)
	}

	if len(h.applyCalls) != 1 {
		t.Fatalf("applies = %v, want exactly 1 (400 = terminal, no second attempt)", h.applyCalls)
	}
	m := mustMetrics(t, h.db)
	if m["apply.skip.apply_failed"] != 1 {
		t.Fatalf("apply.skip.apply_failed = %d, want 1 (metrics %v)", m["apply.skip.apply_failed"], m)
	}
	if h.popupReqs[777032] != 1 {
		t.Fatalf("popup requests = %d, want 1 (run2 must skip before the popup)", h.popupReqs[777032])
	}
}
