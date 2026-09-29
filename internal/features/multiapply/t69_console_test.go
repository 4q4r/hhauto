package multiapply

// T69 console surface over the fake pipeline: the applied/skip/error/
// letter lines, the verdict toggle, the end-of-pass digest and the
// template override — rendered into a buffer through the real pass.

import (
	"bytes"
	"strings"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/console"
	"hhauto/internal/features/featurekit"
)

// consoleDefaults mirrors the config-load defaults for the harness (baseCfg
// bypasses config.Load, so the surface config is spelled out).
func consoleDefaults(mutate func(*config.Console)) config.Console {
	def := true
	cfg := config.Console{
		Applied: &def, Errors: &def, Letters: &def, Lift: &def,
		Skips: "interesting,aggregate",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// withConsole installs a plain console surface (buffer sink) on the
// harness and returns the buffer.
func (h *harness) withConsole(t *testing.T, mutate func(*config.Console)) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	s, err := console.New(buf, false, consoleDefaults(mutate), func(_, hash string) string {
		if hash == "hash1" {
			return "DevOps-инженер Test"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("console.New: %v", err)
	}
	h.console = s
	return buf
}

// T69 §6: the applied line carries title, employer, badges and resume.
func TestConsoleAppliedLineOverFakePipeline(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, nil)
	h.searchItems = []string{
		vacItem(777040, "Backend-разработчик (Go)", 9001, "Corp LLC",
			`"badges":{"badge":[{"type":"employer-it-accreditation"}]}`),
	}
	h.popupByVac[777040] = popupWith(false, false, "ENABLED")

	h.run(t)

	out := buf.String()
	for _, want := range []string{
		"✓ Откликнулся: «Backend-разработчик (Go)» — Corp LLC",
		"[аккредитованный ИТ]",
		"резюме «DevOps-инженер Test»",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("console output %q misses %q", out, want)
		}
	}
	// One applied line, one digest line (one banner per event).
	if got := strings.Count(out, "✓ Откликнулся"); got != 1 {
		t.Fatalf("applied lines = %d, want 1", got)
	}
	if !strings.Contains(out, "✓ Проход: откликов 1 ·") {
		t.Fatalf("digest missing: %q", out)
	}
}

// T69 §6: dry-run renders «Откликнулся бы».
func TestConsoleDryRunVerb(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.General.DryRun = true })
	buf := h.withConsole(t, nil)
	h.searchItems = []string{vacItem(777041, "Go Dev", 9001, "Corp LLC", "")}
	h.popupByVac[777041] = popupWith(false, false, "ENABLED")

	h.run(t)

	if !strings.Contains(buf.String(), "✓ Откликнулся бы: «Go Dev» — Corp LLC") {
		t.Fatalf("dry-run verb missing: %q", buf.String())
	}
	if strings.Contains(buf.String(), "Откликнулся:") {
		t.Fatalf("plain verb leaked into dry-run: %q", buf.String())
	}
}

// T69 §6: the skip modes gate individual lines end-to-end; the digest
// breakdown still counts everything (aggregate). The scored vacancy is
// refused as needs_human — an interesting reason — so interesting/all
// render the line while none/aggregate keep it to the digest.
func TestConsoleSkipModesEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modes    string
		wantLine bool
	}{
		{"none", "none", false},
		{"all", "all", true},
		{"interesting", "interesting", true},
		{"aggregate", "aggregate", false},
		{"combo", "interesting,aggregate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			buf := h.withConsole(t, func(c *config.Console) { c.Skips = tc.modes })
			h.runner.scoring = scoringVerdict(80, "ok", true, false) // ai_needs_human: interesting
			h.searchItems = []string{vacItem(777042, "Go Dev", 9001, "Corp LLC", "")}

			h.run(t)

			out := buf.String()
			if got := strings.Contains(out, "· Пропущен: «Go Dev» — Corp LLC: ai_needs_human"); got != tc.wantLine {
				t.Fatalf("skip line shown = %t, want %t (modes %s): %q", got, tc.wantLine, tc.modes, out)
			}
			if !strings.Contains(out, "пропущено 1 (ИИ 1)") {
				t.Fatalf("digest must still count the skip (modes %s): %q", tc.modes, out)
			}
		})
	}
}

// Routine reasons (filters, dedup, threshold) render only in the digest
// unless the mode is all.
func TestConsoleRoutineSkipHiddenUnlessAll(t *testing.T) {
	build := func(modes string) string {
		h := newHarness(t, nil)
		buf := h.withConsole(t, func(c *config.Console) { c.Skips = modes })
		h.runner.scoring = scoringVerdict(20, "low", false, false) // score_below_threshold: routine
		h.searchItems = []string{vacItem(777043, "Go Dev", 9001, "Corp LLC", "")}
		h.run(t)
		return buf.String()
	}
	for _, modes := range []string{"interesting", "interesting,aggregate", "aggregate", "none"} {
		if out := build(modes); strings.Contains(out, "· Пропущен") {
			t.Fatalf("routine skip shown under %s: %q", modes, out)
		}
	}
	if out := build("all"); !strings.Contains(out, "score_below_threshold") {
		t.Fatalf("all mode lost the routine skip: %q", out)
	}
}

// T69 §6: skips_reasons filters the individual lines over the mode.
func TestConsoleSkipsReasonsEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) {
		c.Skips = "all"
		c.SkipsReasons = []string{"score_below_threshold"}
	})
	h.runner.scoring = scoringVerdict(20, "low", false, false)
	h.searchItems = []string{vacItem(777044, "Go Dev", 9001, "Corp LLC", "")}

	h.run(t)

	out := buf.String()
	if !strings.Contains(out, "· Пропущен: «Go Dev» — Corp LLC: score_below_threshold") {
		t.Fatalf("allowlisted skip line missing: %q", out)
	}
	if strings.Contains(out, "Пропущен") && strings.Count(out, "Пропущен") != 1 {
		t.Fatalf("non-allowlisted skip lines leaked: %q", out)
	}
}

// The digest aggregates a mixed pass (applied, dedup skip, structural
// skip) by category.
func TestConsoleDigestEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "none" })
	// v1 applies; v2 hits the chat-disabled structural skip; v3 hits the
	// dedup (a recorded application from an earlier pass).
	h.searchItems = []string{
		vacItem(777045, "Go Dev 1", 9001, "Corp LLC", ""),
		vacItem(777046, "Go Dev 2", 9002, "Other LLC", ""),
		vacItem(777047, "Go Dev 3", 9003, "Third LLC", ""),
	}
	h.popupByVac[777045] = popupWith(false, false, "ENABLED")
	h.popupByVac[777046] = popupWith(false, false, "DISABLED")
	mustCreateApplication(t, h.db, featurekit.DedupKey("acc1", "hash1", 777047), "applied")

	h.run(t)

	out := buf.String()
	if !strings.Contains(out, "✓ Проход: откликов 1 · писем на проверку 0 · пропущено 2 (дедуп 1, прочее 1) · ошибок 0 · поднято 0") {
		t.Fatalf("digest counters wrong: %q", out)
	}
}

// T69 §6: the verdicts toggle renders classification lines per scored
// vacancy.
func TestConsoleVerdictsEndToEnd(t *testing.T) {
	build := func(verdicts bool) *bytes.Buffer {
		h := newHarness(t, nil)
		buf := h.withConsole(t, func(c *config.Console) {
			c.Verdicts = verdicts
			c.Skips = "none"
		})
		h.searchItems = []string{vacItem(777048, "Go Dev", 9001, "Corp LLC", "")}
		h.popupByVac[777048] = popupWith(false, false, "ENABLED")
		h.run(t)
		return buf
	}
	if out := build(false).String(); strings.Contains(out, "Вердикт:") {
		t.Fatalf("verdicts off but rendered: %q", out)
	}
	out := build(true).String()
	if !strings.Contains(out, "Вердикт: «Go Dev» — score 80, порог 60 → отклик") {
		t.Fatalf("verdict line missing: %q", out)
	}
}

// The needs-human letter renders its own line (letters toggle).
func TestConsoleLetterLine(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "none" })
	h.runner.letter = letterNeedsHumanVerdict()
	h.searchItems = []string{vacItem(777049, "Go Dev", 9001, "Corp LLC", "")}
	h.popupByVac[777049] = popupWith(true, false, "ENABLED")

	h.run(t)

	out := buf.String()
	// The letter event renders from the vacancy in hand at letter-build
	// time — the full card the flow just fetched (richVacancyCard in the
	// fake), not the slimmer search item.
	if !strings.Contains(out, "? Письмо на проверку: «Backend-разработчик (Go)» — Тест-работодатель") {
		t.Fatalf("letter line missing: %q", out)
	}
	if !strings.Contains(out, "писем на проверку 1") {
		t.Fatalf("digest letter count missing: %q", out)
	}
}

// template_applied overrides the applied line wholesale.
func TestConsoleTemplateAppliedEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) {
		c.TemplateApplied = "T69 {{.VacancyTitle}} @ {{.Employer}}"
	})
	h.searchItems = []string{vacItem(777050, "Go Dev", 9001, "Corp LLC", "")}
	h.popupByVac[777050] = popupWith(false, false, "ENABLED")

	h.run(t)

	if !strings.Contains(buf.String(), "T69 Go Dev @ Corp LLC") {
		t.Fatalf("template override not applied: %q", buf.String())
	}
}

// The apply failure renders the ✗ error line.
func TestConsoleErrorLine(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "none" })
	h.searchItems = []string{vacItem(777051, "Go Dev", 9001, "Corp LLC", "")}
	h.popupByVac[777051] = popupWith(false, false, "ENABLED")
	h.applyErr = func(int, int64, string) error {
		return &respErr{status: 500, body: "server exploded"}
	}

	h.run(t)

	out := buf.String()
	if !strings.Contains(out, "✗ Ошибка отклика: «Go Dev» — Corp LLC:") {
		t.Fatalf("error line missing: %q", out)
	}
	if !strings.Contains(out, "ошибок 1") {
		t.Fatalf("digest error count missing: %q", out)
	}
}

// letterNeedsHumanVerdict renders the needs_human letter verdict JSON.
func letterNeedsHumanVerdict() string {
	return `{"confidence":0.9,"needs_human":true,"needs_human_reason":"policy","injection_suspected":false,"letter":"","used_facts":[],"missing_data":[]}`
}
