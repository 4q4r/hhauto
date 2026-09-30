package multiapply

// T79: the letter path against the drifted hh card shape. The card content
// lives in vacancyView.vacancyFull.vacancy (the flat vacancyView carries
// nothing — the 2026-09-30 live probe): a letter must build from the nested
// card in the same single fetch, and a card that is empty through and
// through must skip fail-closed BEFORE the AI call.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/config"
)

// nestedVacancyFullCard is the fake hh card in the live-probed nested shape
// (the flat vacancyView the fake wraps carries no content of its own).
func nestedVacancyFullCard(id int64) string {
	return fmt.Sprintf(`{"vacancyFull":{"vacancy":{"vacancyId":%d,`+
		`"name":"Nested DevOps","description":"<p>Инфраструктура и <b>платформы</b>.</p>",`+
		`"publicationTimeIso":"2026-09-30T09:23:35.403+03:00",`+
		`"company":{"id":9003,"name":"Corp Notes"},`+
		`"area":{"name":"Москва"},"compensation":{"from":200000,"currencyCode":"RUR"},`+
		`"key_skills":[{"name":"Go"}]}}}`, id)
}

// The letter builds from the nested card in ONE fetch: no second endpoint,
// no re-fetch; the parsed title, employer, description and publication date
// all ride into the AI letter input.
func TestCoverLetterNestedVacancyFullCardT79(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		coverLetterSection(c, nil) // mode "" behaves as ai
	})
	twoVacancies(h)
	h.cardByVac[101] = nestedVacancyFullCard(101)
	// 102 keeps the legacy flat rich card (mixed shapes in one run) and
	// applies without a letter: the only cover_letter call then belongs to
	// vacancy 101's nested card.
	h.popupByVac[101] = popupWith(true, false, "ENABLED")
	h.popupByVac[102] = popupWith(false, false, "ENABLED")
	f := h.feature(t)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := h.applyCalls; len(got) != 2 {
		t.Fatalf("applyCalls = %v, want both vacancies applied", got)
	}
	if got := h.cardReqs[101]; got != 1 {
		t.Fatalf("vacancy 101 card fetched %d times, want exactly 1 (no second path calls)", got)
	}
	m := lastLetterInput(t, h)
	details, ok := m["vacancy_details"].(map[string]any)
	if !ok {
		t.Fatalf("letter input misses vacancy_details: %v", m)
	}
	for field, want := range map[string]string{
		"title":         "Nested DevOps",
		"employer_name": "Corp Notes",
		"area":          "Москва",
		"key_skills":    "Go",
	} {
		if got, _ := details[field].(string); got != want {
			t.Errorf("vacancy_details.%s = %q, want %q", field, got, want)
		}
	}
	if got, _ := details["description"].(string); got == "" || strings.Contains(got, "<p>") {
		t.Errorf("vacancy_details.description = %q, want non-empty plain text", got)
	}
	vac, ok := m["vacancy"].(map[string]any)
	if !ok {
		t.Fatalf("letter input misses the vacancy block: %v", m)
	}
	if pub, _ := vac["published_at"].(string); pub == "" {
		t.Errorf("letter input vacancy.published_at = empty, want the publicationTimeIso date")
	}
}

// A card that is empty through and through (the vacancyFull block empty OR
// a bare vacancyView) skips fail-closed before any AI call.
func TestCoverLetterEmptyCardSkipsBeforeAIT79(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		coverLetterSection(c, nil)
	})
	twoVacancies(h)
	h.popupByVac[101] = popupWith(true, false, "ENABLED")
	h.popupByVac[102] = popupWith(true, false, "ENABLED")
	h.cardByVac[101] = `{"vacancyFull":{"vacancy":{}}}` // nested path empty
	h.cardByVac[102] = `{}`                             // flat path empty
	f := h.feature(t)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := h.applyCalls; len(got) != 0 {
		t.Fatalf("applyCalls = %v, want none (empty cards skip)", got)
	}
	if m := metricsOf(t, h.db); m["apply.skip.vacancy_card_failed"] != 2 {
		t.Fatalf("apply.skip.vacancy_card_failed = %v, want 2", m["apply.skip.vacancy_card_failed"])
	}
	for _, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			t.Fatalf("cover_letter called on an empty card: %v", h.runner.calls)
		}
	}
}
