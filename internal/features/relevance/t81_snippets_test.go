package relevance

// T81: the scorer's untrusted input carries the typed search snippets and no
// longer drags the whole raw search item behind them (the letter path strips
// Vacancy.Raw for the same reason: it duplicates the dump and burns the
// token budget). Cache versioning (v4) re-scores verdicts that were made
// without snippets.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The scoring input json carries the typed snippet fields; the raw item blob
// (which also embeds the snippets) must not ride along.
func TestScoringInputCarriesSnippetsWithoutRaw(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(80, "ok", false, false)}
	f := newFilter(t, db, runner)

	v := vacancy(801, "DevOps-инженер")
	v.SnippetRequirement = "Опыт: от 3 лет, Jenkins, Kubernetes"
	v.SnippetResponsibility = "Автоматизация сборки и доставки"
	v.SnippetSkill = "Jenkins, Docker, Kubernetes"
	v.Raw = json.RawMessage(`{"snippet":{"req":"dup","skill":"dup"},"contactInfo":{"name":"x"}}`)

	if _, _, _ = f.Pass(context.Background(), v, "hash1", 60); runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if !strings.Contains(runner.input, `"snippet_requirement":"Опыт: от 3 лет, Jenkins, Kubernetes"`) {
		t.Errorf("scoring input lacks the typed snippet requirement: %s", runner.input)
	}
	if strings.Contains(runner.input, `"raw":`) {
		t.Errorf("scoring input still drags the raw item blob: %s", runner.input)
	}
}

// Verdicts cached under the pre-T81 key (v3: scored without snippets) must
// not serve: the feature_state cache has no TTL, so the whole live pool
// would keep its snippet-less verdicts forever.
func TestStalePreT81CacheEntryIgnored(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, key := range []string{"802:hash1", "v3:802:hash1"} {
		if err := db.FeatureStateSet(ctx, feature, "acc", key, `{"score":80,"band":"ok","needs_human":true}`); err != nil {
			t.Fatalf("seed stale cache entry %s: %v", key, err)
		}
	}
	runner := &fakeRunner{verdict: scoringVerdict(80, "ok", false, false)}
	f := newFilter(t, db, runner)

	if _, pass, _ := f.Pass(ctx, vacancy(802, "Go Dev"), "hash1", 60); !pass {
		t.Fatalf("pass = false, want true (fresh scoring, not a stale entry)")
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (stale keys must miss)", runner.calls)
	}
}
