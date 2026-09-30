package multiapply

import "testing"

// T71 fail-closed wiring: ai_filter=true with an UNAVAILABLE runner must
// skip vacancies with a reason, never pass them unscored. The 2026-09-30
// night applied to two Erlang vacancies while the scorer was silently off.
func TestAIFilterWithoutRunnerSkipsVacancies(t *testing.T) {
	h := newHarness(t, nil)
	h.noRunner = true
	h.searchItems = []string{vacItem(777099, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if len(h.applyCalls) != 0 || h.popupReqs[777099] != 0 {
		t.Fatalf("applies=%v popupReqs=%v, want none (fail-closed precedes popup)", h.applyCalls, h.popupReqs)
	}
	if mustMetrics(t, h.db)["apply.skip.ai_unavailable"] != 1 {
		t.Fatalf("metrics = %v, want apply.skip.ai_unavailable=1", mustMetrics(t, h.db))
	}
}
