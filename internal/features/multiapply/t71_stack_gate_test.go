package multiapply

import "testing"

// T71 hard pre-AI stack gate: a vacancy whose NAME names languages that are
// all absent from the candidate's profile text is skipped deterministically,
// before any AI scoring, popups or letters. The 2026-09-30 night applied to
// two Erlang vacancies while the scorer was off.

func TestStackGateSkipsForeignLanguageVacancy(t *testing.T) {
	h := newHarness(t, nil)
	h.profile.ProfileText = "Стек: Go, Python, Rust. Целевые роли: backend, devops."
	h.searchItems = []string{vacItem(777101, "Erlang-разработчик в блокчейн-проект", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if len(h.applyCalls) != 0 || h.popupReqs[777101] != 0 {
		t.Fatalf("applies=%v popupReqs=%v, want none (stack gate precedes popup)", h.applyCalls, h.popupReqs)
	}
	if mustMetrics(t, h.db)["apply.skip.stack_mismatch"] != 1 {
		t.Fatalf("metrics = %v, want apply.skip.stack_mismatch=1", mustMetrics(t, h.db))
	}
}

func TestStackGatePassesIntersectingStack(t *testing.T) {
	h := newHarness(t, nil)
	h.profile.ProfileText = "Стек: Go, Python, Rust."
	h.searchItems = []string{vacItem(777102, "Backend-разработчик (Python, Kafka)", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if mustMetrics(t, h.db)["apply.skip.stack_mismatch"] != 0 {
		t.Fatalf("metrics = %v, want no stack_mismatch (python intersects)", mustMetrics(t, h.db))
	}
}

func TestStackGatePassesUnmarkedName(t *testing.T) {
	h := newHarness(t, nil)
	h.profile.ProfileText = "Стек: Go, Python, Rust."
	h.searchItems = []string{vacItem(777103, "Старший инженер автоматизации", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if mustMetrics(t, h.db)["apply.skip.stack_mismatch"] != 0 {
		t.Fatalf("metrics = %v, want no stack_mismatch (no language markers)", mustMetrics(t, h.db))
	}
}

func TestStackGateOffWithoutStackInProfile(t *testing.T) {
	h := newHarness(t, nil)
	h.profile.ProfileText = "Ищу инженерные роли." // no known language in the profile
	h.searchItems = []string{vacItem(777104, "Erlang-разработчик", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if mustMetrics(t, h.db)["apply.skip.stack_mismatch"] != 0 {
		t.Fatalf("metrics = %v, want no stack_mismatch (profile carries no stack: gate inactive)", mustMetrics(t, h.db))
	}
}
