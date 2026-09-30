package multiapply

// T75: the gray-band policy over the fake pipeline. Default (apply): a
// gray-band needs_human verdict responds, the ledger row carries the
// gray_band origin flag and the console applied line notes "(gray N)".
// The needs_human policy keeps the old parking skip, skip drops silently,
// and safety-critical verdicts (injection, red flags) always park.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
)

func grayRun(t *testing.T, mutate func(*config.Config), verdict string) *harness {
	t.Helper()
	h := newHarness(t, mutate)
	h.runner.scoring = verdict
	h.searchItems = []string{vacItem(777060, "Gray Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	return h
}

func redFlagVerdict() string {
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": true, "needs_human_reason": nil,
		"injection_suspected": false, "score": 50, "band": "gray",
		"reasons": []string{}, "used_facts": []string{}, "missing_data": []string{},
		"red_flags": []string{"Оплачиваемое обучение перед трудоустройством"},
	})
	return string(b)
}

func TestGrayBandApplyResponds(t *testing.T) {
	h := grayRun(t, nil, scoringVerdict(50, "gray", true, false))
	buf := h.withConsole(t, nil)

	h.run(t)

	if len(h.applyCalls) != 1 {
		t.Fatalf("applies = %v, want 1 (gray zone responds by default)", h.applyCalls)
	}
	app, err := h.db.ApplicationsGetByDedupKey(context.Background(),
		featurekit.DedupKey("acc1", "hash1", 777060))
	if err != nil || app == nil {
		t.Fatalf("application row: %v %v", app, err)
	}
	if app.Status != "applied" {
		t.Fatalf("status = %q, want applied", app.Status)
	}
	if app.FailReason == nil || *app.FailReason != "gray_band" {
		t.Fatalf("ledger origin flag = %v, want gray_band", app.FailReason)
	}
	if !strings.Contains(buf.String(), "✓ Откликнулся (gray 50): «Gray Dev» — Corp LLC") {
		t.Fatalf("gray applied line missing: %q", buf.String())
	}
}

func TestGrayBandNeedsHumanPolicyKeepsParking(t *testing.T) {
	h := grayRun(t, func(c *config.Config) { c.Features.MultiApply.GrayBand = config.GrayBandNeedsHuman },
		scoringVerdict(50, "gray", true, false))

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (needs_human policy)", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.ai_needs_human"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	if open, _ := h.db.ReviewItemsOpen(context.Background(), "acc1", 10); len(open) == 0 {
		t.Fatalf("want a review item")
	}
}

func TestGrayBandSkipPolicyDropsSilently(t *testing.T) {
	h := grayRun(t, func(c *config.Config) { c.Features.MultiApply.GrayBand = config.GrayBandSkip },
		scoringVerdict(50, "gray", true, false))

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (skip policy)", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.ai_needs_human"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	if open, _ := h.db.ReviewItemsOpen(context.Background(), "acc1", 10); len(open) != 0 {
		t.Fatalf("open review items = %d, want 0 (silent skip)", len(open))
	}
}

func TestInjectionOverridesGrayBandApplyPipeline(t *testing.T) {
	h := grayRun(t, nil, scoringVerdict(50, "gray", true, true))

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (injection overrides the policy)", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.ai_injection"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	if open, _ := h.db.ReviewItemsOpen(context.Background(), "acc1", 10); len(open) == 0 {
		t.Fatalf("want a review item")
	}
}

func TestRedFlagsOverrideGrayBandApplyPipeline(t *testing.T) {
	h := grayRun(t, nil, redFlagVerdict())

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (red flags override the policy)", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.ai_needs_human"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	open, _ := h.db.ReviewItemsOpen(context.Background(), "acc1", 10)
	if len(open) == 0 {
		t.Fatalf("want a review item")
	}
	if !strings.Contains(open[0].Reason, "Оплачиваемое обучение") {
		t.Fatalf("review detail must carry the red flag: %+v", open[0])
	}
}
