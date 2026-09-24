package profile

import (
	"strings"
	"testing"
)

func sampleProfile() Profile {
	return Profile{
		AccountID:    "acc",
		ResumeDigest: "Резюме «Go-разработчик»\nРегион: Москва",
		ProfileText:  "Люблю Go, не упоминать возраст.",
		Facts: map[string]string{
			"Вилка?":     "от 250000 RUR",
			"Релокация?": "только Москва и СПб",
			"Формат":     "гибрид",
		},
	}
}

func TestRenderForPromptAllSections(t *testing.T) {
	got := RenderForPrompt(sampleProfile())
	assertContains(t, got,
		"### ПРОФИЛЬ КАНДИДАТА",
		"Люблю Go, не упоминать возраст.",
		"### РЕЗЮМЕ",
		"Резюме «Go-разработчик»",
		"### ФАКТЫ ПОЛЬЗОВАТЕЛЯ",
		"В: Вилка?\nО: от 250000 RUR",
	)
	// Canonical order regardless of map iteration.
	if i1, i2, i3 := strings.Index(got, "ПРОФИЛЬ КАНДИДАТА"), strings.Index(got, "### РЕЗЮМЕ"), strings.Index(got, "ФАКТЫ ПОЛЬЗОВАТЕЛЯ"); i1 >= i2 || i2 >= i3 {
		t.Fatalf("sections out of canonical order:\n%s", got)
	}
}

func TestRenderForPromptDeterministic(t *testing.T) {
	a := RenderForPrompt(sampleProfile())
	b := RenderForPrompt(sampleProfile())
	if a != b {
		t.Fatalf("render is not deterministic:\n---\n%s\n---\n%s", a, b)
	}
}

func TestRenderForPromptSectionSelection(t *testing.T) {
	p := sampleProfile()
	onlyResume := RenderForPrompt(p, SectionResume)
	assertContains(t, onlyResume, "### РЕЗЮМЕ")
	assertNotContains(t, onlyResume, "ПРОФИЛЬ КАНДИДАТА", "ФАКТЫ ПОЛЬЗОВАТЕЛЯ")

	// Selection order does not affect the output order.
	a := RenderForPrompt(p, SectionFacts, SectionProfile)
	b := RenderForPrompt(p, SectionProfile, SectionFacts)
	if a != b {
		t.Fatalf("selection order changed output:\n---\n%s\n---\n%s", a, b)
	}
	if i1, i2 := strings.Index(a, "ПРОФИЛЬ КАНДИДАТА"), strings.Index(a, "ФАКТЫ ПОЛЬЗОВАТЕЛЯ"); i1 > i2 {
		t.Fatalf("profile section must precede facts:\n%s", a)
	}
}

func TestRenderForPromptSkipsEmptySections(t *testing.T) {
	empty := Profile{AccountID: "acc"}
	if got := RenderForPrompt(empty); got != "" {
		t.Fatalf("render of empty profile = %q, want empty string", got)
	}
	resumeOnly := Profile{AccountID: "acc", ResumeDigest: "Резюме «X»"}
	got := RenderForPrompt(resumeOnly)
	assertContains(t, got, "### РЕЗЮМЕ\nРезюме «X»")
	assertNotContains(t, got, "ПРОФИЛЬ КАНДИДАТА", "ФАКТЫ ПОЛЬЗОВАТЕЛЯ")
}

func TestRenderForPromptFactsSorted(t *testing.T) {
	p := Profile{AccountID: "acc", Facts: map[string]string{"б": "2", "а": "1"}}
	got := RenderForPrompt(p, SectionFacts)
	if i1, i2 := strings.Index(got, "В: а"), strings.Index(got, "В: б"); i1 > i2 {
		t.Fatalf("facts not sorted by question:\n%s", got)
	}
	assertContains(t, got, "В: а\nО: 1", "В: б\nО: 2")
}

func TestRenderForPromptTruncatesAtLineBoundary(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "line"+string(rune('0'+i))+" padding...")
	}
	long := strings.Join(lines, "\n")

	got := RenderForPromptLimit(Profile{AccountID: "acc", ProfileText: long}, 40, SectionProfile)
	assertContains(t, got, "### ПРОФИЛЬ КАНДИДАТА\n")
	assertContains(t, got, lines[0], lines[1])
	assertNotContains(t, got, lines[2], lines[9])
	assertContains(t, got, "усечено")
	// The cut happens on a line boundary: the marker starts a fresh line.
	if !strings.HasSuffix(got, "\n"+truncationMarker) {
		t.Fatalf("marker must start on a new line, got suffix %q", got[len(got)-20:])
	}
}

func TestRenderForPromptHardCutSingleLine(t *testing.T) {
	long := strings.Repeat("а", 100)
	got := RenderForPromptLimit(Profile{AccountID: "acc", ProfileText: long}, 10, SectionProfile)
	assertContains(t, got, strings.Repeat("а", 10))
	assertNotContains(t, got, strings.Repeat("а", 11))
	assertContains(t, got, "усечено")
}

func TestRenderForPromptLimitZeroDisablesTruncation(t *testing.T) {
	long := strings.Repeat("x", 500) + "\n" + strings.Repeat("y", 500)
	got := RenderForPromptLimit(Profile{AccountID: "acc", ProfileText: long}, 0, SectionProfile)
	if !strings.Contains(got, strings.Repeat("y", 500)) {
		t.Fatal("limit <= 0 must disable truncation")
	}
	assertNotContains(t, got, "усечено")
}

func TestRenderForPromptTruncatesFactsBlock(t *testing.T) {
	facts := map[string]string{}
	for i := 0; i < 50; i++ {
		facts["Вопрос номер "+string(rune('А'+i))+"?"] = "ответ ответ ответ"
	}
	got := RenderForPromptLimit(Profile{AccountID: "acc", Facts: facts}, 120, SectionFacts)
	assertContains(t, got, "усечено")
}
