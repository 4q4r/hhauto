package relevance

// T78: needs_human is criticism only. A schema-valid verdict that sets
// needs_human=true without criticism (suspected injection or a data-dummy
// vacancy) is prompt drift: the filter demotes it to the gray band whose
// fate the gray-band config decides (T75). Bandless verdicts are runner
// synthetics (budget, schema-invalid) and keep the fail-closed skip, and
// the verdict cache is version-keyed so pre-T78 verdicts are re-scored.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// driftVerdict is a schema-valid verdict that escalates without criticism:
// band ok + needs_human=true — the v2 prompt pattern this task removes.
func driftVerdict() string {
	return scoringVerdict(80, "ok", true, false)
}

func TestDriftNeedsHumanDemotedToGrayBand(t *testing.T) {
	db := testDB(t)
	var buf bytes.Buffer
	runner := &fakeRunner{verdict: driftVerdict()}
	f := newFilter(t, db, runner)
	f.log = slog.New(slog.NewTextHandler(&buf, nil))

	v, pass, reason := f.Pass(context.Background(), vacancy(791, "Go Dev"), "hash1", 60)
	if !pass || reason != GrayBandReason {
		t.Fatalf("pass=%v reason=%q, want true/%s (demoted into the gray policy)", pass, reason, GrayBandReason)
	}
	if v.Band != BandGray {
		t.Fatalf("band = %q, want gray (needs_human without criticism is drift)", v.Band)
	}
	if !strings.Contains(buf.String(), "demoted to gray band") {
		t.Fatalf("demotion log line missing: %q", buf.String())
	}
}

func TestDummyVacancyKeepsNeedsHuman(t *testing.T) {
	db := testDB(t)
	var buf bytes.Buffer
	runner := &fakeRunner{verdict: driftVerdict()}
	f := newFilter(t, db, runner)
	f.log = slog.New(slog.NewTextHandler(&buf, nil))

	_, pass, reason := f.Pass(context.Background(), vacancy(792, ""), "hash1", 60)
	if pass || reason != "ai_needs_human" {
		t.Fatalf("pass=%v reason=%q, want false/ai_needs_human (a dummy vacancy is criticism)", pass, reason)
	}
	if strings.Contains(buf.String(), "demoted") {
		t.Fatalf("dummy-vacancy verdict must not be demoted: %q", buf.String())
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 1 {
		t.Fatalf("open review items = %d, want 1", len(open))
	}
}

func TestBandlessSyntheticVerdictNotDemoted(t *testing.T) {
	db := testDB(t)
	// The runner's synthetic verdict (budget exhausted, schema invalid)
	// carries needs_human=true without a band: it must keep the
	// fail-closed skip, never become an applicable gray verdict.
	runner := &fakeRunner{verdict: `{"confidence":0,"needs_human":true,"needs_human_reason":"budget_exhausted","injection_suspected":false}`}
	f := newFilter(t, db, runner) // gray_band = apply (the default)

	_, pass, reason := f.Pass(context.Background(), vacancy(793, "Go Dev"), "hash1", 60)
	if pass || reason != "ai_needs_human" {
		t.Fatalf("pass=%v reason=%q, want false/ai_needs_human (fail-closed)", pass, reason)
	}
}

func TestStalePreT78CacheEntryIgnored(t *testing.T) {
	db := testDB(t)
	// A verdict cached under the pre-T78 key (no policy version) must not
	// serve: the flooded v2 night would otherwise route from cache forever
	// (feature_state has no TTL) instead of being re-scored under v3.
	ctx := context.Background()
	if err := db.FeatureStateSet(ctx, feature, "acc", "794:hash1", `{"score":80,"band":"ok","needs_human":true}`); err != nil {
		t.Fatalf("seed legacy cache entry: %v", err)
	}
	runner := &fakeRunner{verdict: scoringVerdict(80, "ok", false, false)}
	f := newFilter(t, db, runner)

	if _, pass, _ := f.Pass(ctx, vacancy(794, "Go Dev"), "hash1", 60); !pass {
		t.Fatalf("pass = false, want true (fresh scoring, not the stale entry)")
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (the legacy key must miss)", runner.calls)
	}
}
