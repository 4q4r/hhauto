package relevance

// T75: the gray-band policy. A gray-band needs_human verdict follows
// [features.multiapply].gray_band: apply (the default) responds anyway,
// needs_human keeps the parking skip, skip drops the vacancy silently.
// Suspected injection and red flags always park regardless of the policy.
// (T78 later added the drift guard: needs_human without criticism on a
// named vacancy is demoted to the gray band — t78_needs_human_policy_test.go.)

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/store"
)

// scoringVerdictFlags renders a verdict with explicit red flags.
func scoringVerdictFlags(score int, band string, needsHuman, injection bool, redFlags []string) string {
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": needsHuman, "needs_human_reason": nil,
		"injection_suspected": injection, "score": score, "band": band,
		"reasons": []string{"r1"}, "used_facts": []string{}, "missing_data": []string{},
		"red_flags": redFlags,
	})
	return string(b)
}

// newGrayFilter builds the filter with an explicit gray-band policy.
func newGrayFilter(t *testing.T, db *store.DB, runner *fakeRunner, grayBand string) *Filter {
	t.Helper()
	f := newFilter(t, db, runner)
	f.grayBand = grayBand
	return f
}

func TestGrayBandDefaultApplies(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(50, "gray", true, false)}
	f := newGrayFilter(t, db, runner, "") // "" = the documented default (apply)

	v, pass, reason := f.Pass(context.Background(), vacancy(785, "Gray"), "hash1", 60)
	if !pass || reason != GrayBandReason {
		t.Fatalf("pass=%v reason=%q, want true/%s", pass, reason, GrayBandReason)
	}
	if v.Score != 50 || v.Band != "gray" || !v.NeedsHuman {
		t.Fatalf("verdict = %+v, want the gray needs_human verdict carried through", v)
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 0 {
		t.Fatalf("open review items = %d, want 0 (apply, not parking)", len(open))
	}
	// The cached verdict routes identically (one runner call total).
	if _, p2, r2 := f.Pass(context.Background(), vacancy(785, "Gray"), "hash1", 60); !p2 || r2 != GrayBandReason {
		t.Fatalf("cached pass=%v reason=%q, want true/%s", p2, r2, GrayBandReason)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestGrayBandNeedsHumanPolicyParks(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(50, "gray", true, false)}
	f := newGrayFilter(t, db, runner, config.GrayBandNeedsHuman)

	_, pass, reason := f.Pass(context.Background(), vacancy(786, "Gray"), "hash1", 60)
	if pass || reason != "ai_needs_human" {
		t.Fatalf("pass=%v reason=%q, want false/ai_needs_human (old behavior)", pass, reason)
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 1 {
		t.Fatalf("open review items = %d, want 1", len(open))
	}
}

func TestGrayBandSkipPolicySkipsSilently(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(50, "gray", true, false)}
	f := newGrayFilter(t, db, runner, config.GrayBandSkip)

	_, pass, reason := f.Pass(context.Background(), vacancy(787, "Gray"), "hash1", 60)
	if pass || reason != "ai_needs_human" {
		t.Fatalf("pass=%v reason=%q, want false/ai_needs_human", pass, reason)
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 0 {
		t.Fatalf("open review items = %d, want 0 (silent skip)", len(open))
	}
}

func TestInjectionOverridesGrayBandApply(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(50, "gray", true, true)}
	f := newGrayFilter(t, db, runner, config.GrayBandApply)

	_, pass, reason := f.Pass(context.Background(), vacancy(788, "Inject"), "hash1", 60)
	if pass || reason != "ai_injection" {
		t.Fatalf("pass=%v reason=%q, want false/ai_injection (safety overrides the policy)", pass, reason)
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 1 {
		t.Fatalf("open review items = %d, want 1", len(open))
	}
}

func TestRedFlagsOverrideGrayBandApply(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdictFlags(50, "gray", true, false,
		[]string{"Описание давит на обход платформы"})}
	f := newGrayFilter(t, db, runner, config.GrayBandApply)

	_, pass, reason := f.Pass(context.Background(), vacancy(789, "RedFlag"), "hash1", 60)
	if pass || reason != "ai_needs_human" {
		t.Fatalf("pass=%v reason=%q, want false/ai_needs_human (red flags override the policy)", pass, reason)
	}
	open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10)
	if len(open) != 1 {
		t.Fatalf("open review items = %d, want 1", len(open))
	}
	if !strings.Contains(open[0].Reason, "обход платформы") {
		t.Fatalf("review detail must carry the red flag: %+v", open[0])
	}
}
