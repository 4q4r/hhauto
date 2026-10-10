// T116: spintax rendering ({variant1|variant2}) of template cover letters.
// The transform is a pure function over a seeded *rand.Rand, resolves at
// most two levels of nesting, leaves placeholder-like and group-less text
// verbatim, and hooks only into the template / template-fallback branches
// of buildLetter — AI letters must pass through untouched.

package multiapply

import (
	"context"
	"math/rand/v2"
	"testing"

	"hhauto/internal/config"
)

// applyLetters decodes the letter field of every recorded apply body.
func applyLetters(t *testing.T, h *harness) []string {
	t.Helper()
	letters := make([]string, 0, len(h.applyBodies))
	for _, body := range h.applyBodies {
		form, err := parseForm(body)
		if err != nil {
			t.Fatalf("parse apply body: %v", err)
		}
		letters = append(letters, form["letter"])
	}
	return letters
}

func TestRenderSpintaxDeterministicGolden(t *testing.T) {
	in := "Стек: {Go|Python}, уровень {junior|middle{1|2}}."
	got := renderSpintax(in, rand.New(rand.NewPCG(42, 7)))
	if again := renderSpintax(in, rand.New(rand.NewPCG(42, 7))); got != again {
		t.Fatalf("seeded render is not deterministic: %q vs %q", got, again)
	}
	valid := map[string]bool{
		"Стек: Go, уровень junior.":      true,
		"Стек: Go, уровень middle1.":     true,
		"Стек: Go, уровень middle2.":     true,
		"Стек: Python, уровень junior.":  true,
		"Стек: Python, уровень middle1.": true,
		"Стек: Python, уровень middle2.": true,
	}
	if !valid[got] {
		t.Fatalf("renderSpintax = %q, want one of the 6 valid combinations", got)
	}
	const golden = "Стек: Python, уровень middle1."
	if got != golden {
		t.Fatalf("golden: got %q, want %q", got, golden)
	}
}

func TestRenderSpintaxVerbatimCases(t *testing.T) {
	cases := []string{
		"",
		"Обычный текст остаётся прежним.",
		"Плейсхолдер {{.VacancyTitle}} не трогается.",
		"{без вариантов} остаётся литералом.",
		"Незакрытая {группа тоже",
		"Пайп снаружи | скобок",
	}
	for _, in := range cases {
		if got := renderSpintax(in, rand.New(rand.NewPCG(1, 2))); got != in {
			t.Errorf("renderSpintax(%q) = %q, want verbatim", in, got)
		}
	}
}

func TestRenderSpintaxNestingLimit(t *testing.T) {
	// The depth-2 group {1|{2|3}} resolves; a picked third-level group
	// {2|3} must survive as literal text, never resolve. Seed PCG(6,9)
	// deterministically lands on the revealing "b{2|3}" branch.
	switch got := renderSpintax("{a|b{1|{2|3}}}", rand.New(rand.NewPCG(6, 9))); got {
	case "a", "b1", "b{2|3}":
	default:
		t.Fatalf("render = %q, want a | b1 | b{2|3} (third level literal)", got)
	}
}

func TestSpinAppliesToTemplateAndFallbackOnly(t *testing.T) {
	// Template mode: the letter is spun, the raw group never ships.
	h := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.CoverLetter.Spin = true
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplate
		run.LetterTemplate = "Привет, {джун|мид}!"
		c.Runs["run1"] = run
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	letters := applyLetters(t, h)
	if len(letters) != 2 {
		t.Fatalf("letters = %d, want 2", len(letters))
	}
	for i, l := range letters {
		if l != "Привет, джун!" && l != "Привет, мид!" {
			t.Fatalf("letter %d = %q, want a spun variant", i, l)
		}
	}

	// template_ai: the polished letter is verbatim even when the model
	// emits brace pairs.
	h2 := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.AIFilter = false
		c.Features.MultiApply.CoverLetter.Spin = true
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplateAI
		run.LetterTemplate = "БАЗА {A|B}"
		c.Runs["run1"] = run
	})
	h2.runner.letter = letterVerdict("ОТПОЛИРОВАНО {X|Y}")
	twoVacancies(h2)
	f2 := buildCloneFeature(t, h2, nil)
	if err := f2.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile polish: %v", err)
	}
	if got := applyLetters(t, h2)[0]; got != "ОТПОЛИРОВАНО {X|Y}" {
		t.Fatalf("polished AI letter = %q, want verbatim", got)
	}

	// template_ai fallback (AI unavailable): the raw template is spun.
	h3 := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.AIFilter = false
		c.Features.MultiApply.CoverLetter.Spin = true
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplateAI
		run.LetterTemplate = "БАЗА {A|B}"
		c.Runs["run1"] = run
	})
	h3.runner.err = context.DeadlineExceeded
	twoVacancies(h3)
	f3 := buildCloneFeature(t, h3, nil)
	if err := f3.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile fallback: %v", err)
	}
	switch got := applyLetters(t, h3)[0]; got {
	case "БАЗА A", "БАЗА B":
	default:
		t.Fatalf("fallback letter = %q, want a spun variant", got)
	}
}

func TestSpinNeverTouchesAILetters(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.CoverLetter.Spin = true
		run := c.Runs["run1"]
		// The template is the AI context in ai_company mode; the AI letter
		// itself must ship verbatim, brace pairs included.
		run.LetterTemplate = "КОНТЕКСТ"
		c.Runs["run1"] = run
	})
	h.runner.letter = letterVerdict("ИИ-письмо с {литеральными|скобками}")
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	// Default letter mode is ai_company: a successful AI letter is shipped
	// verbatim, brace pairs included.
	if got := applyLetters(t, h)[0]; got != "ИИ-письмо с {литеральными|скобками}" {
		t.Fatalf("AI letter = %q, want verbatim", got)
	}
}

func TestSpinOffByDefault(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplate
		run.LetterTemplate = "ШАБЛОН {A|B}"
		c.Runs["run1"] = run
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := applyLetters(t, h)[0]; got != "ШАБЛОН {A|B}" {
		t.Fatalf("spin-off letter = %q, want the verbatim template", got)
	}
}
