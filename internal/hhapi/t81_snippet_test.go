package hhapi

// T81: search snippets become typed Vacancy fields so the scorer decides on
// the vacancy's substance (requirements / responsibilities / stack), not the
// title alone. Golden over the same depersonalized live search item the
// badges test uses (testdata/live/search_item_badges.json).

import (
	"encoding/json"
	"strings"
	"testing"
)

// T81 golden: the live search item's snippet.{req,resp,skill} fill the typed
// fields verbatim (plain text, no markup in the capture).
func TestVacancySnippetsFromLiveSearchItem(t *testing.T) {
	v := vacancyFromItem(json.RawMessage(readLive(t, "search_item_badges.json")))
	if !strings.Contains(v.SnippetRequirement, "Опыт автоматизации инфраструктуры") {
		t.Errorf("SnippetRequirement = %q", v.SnippetRequirement)
	}
	if !strings.Contains(v.SnippetResponsibility, "Автоматизация сборки и доставки") {
		t.Errorf("SnippetResponsibility = %q", v.SnippetResponsibility)
	}
	if !strings.Contains(v.SnippetSkill, "Jenkins") || !strings.Contains(v.SnippetSkill, "Kubernetes") {
		t.Errorf("SnippetSkill = %q", v.SnippetSkill)
	}
}

// Snippet normalization: markup stripped, HTML entities decoded, whitespace
// collapsed to single spaces, every field capped at snippetCap runes.
func TestSnippetTextNormalizesTagsEntitiesAndCaps(t *testing.T) {
	raw := `{"snippet":{
		"req":"Опыт: <b>от 3 лет</b> &nbsp; с Docker&nbsp;&amp; Kubernetes.",
		"resp":"Настройка CI/CD,\n    поддержка пайплайнов.",
		"skill":"` + strings.Repeat("Go, PostgreSQL, ", 60) + `Docker"
	}}`
	v := vacancyFromItem(json.RawMessage(raw))

	if v.SnippetRequirement != "Опыт: от 3 лет с Docker & Kubernetes." {
		t.Errorf("SnippetRequirement = %q", v.SnippetRequirement)
	}
	if v.SnippetResponsibility != "Настройка CI/CD, поддержка пайплайнов." {
		t.Errorf("SnippetResponsibility = %q", v.SnippetResponsibility)
	}
	if got := []rune(v.SnippetSkill); len(got) > snippetCap+1 {
		t.Errorf("SnippetSkill not capped: %d runes", len(got))
	}
	if !strings.HasSuffix(v.SnippetSkill, "…") {
		t.Errorf("capped snippet must end with the ellipsis mark, got %q", v.SnippetSkill[len(v.SnippetSkill)-10:])
	}
}

// No snippet node (JSON-card vacancies) → empty fields, no crash.
func TestVacancyWithoutSnippetStaysEmpty(t *testing.T) {
	v := vacancyFromItem(json.RawMessage(`{"vacancyId":1,"name":"x","snippet":{"req":"  "}}`))
	if v.SnippetRequirement != "" || v.SnippetResponsibility != "" || v.SnippetSkill != "" {
		t.Errorf("snippet fields = %q/%q/%q, want all empty", v.SnippetRequirement, v.SnippetResponsibility, v.SnippetSkill)
	}
}
