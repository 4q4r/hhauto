package console

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
)

// testCfg builds the surface config: T69 defaults (line types on, skips
// interesting+aggregate) with the mutator applied.
func testCfg(mutate func(*config.Console)) config.Console {
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

func newTestSurface(t *testing.T, color bool, cfg config.Console) (*Surface, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	s, err := newSurface(buf, color, cfg, nil)
	if err != nil {
		t.Fatalf("newSurface: %v", err)
	}
	return s, buf
}

func vacancy(badgesRaw string) hhapi.Vacancy {
	v := hhapi.Vacancy{
		ID: 777001, Name: "Backend-разработчик (Go)", EmployerName: "Corp LLC",
		Area:   "Таганрог",
		Salary: &hhapi.Salary{From: 150000, To: 250000, Currency: "RUR"},
	}
	if badgesRaw != "" {
		v.Badges = hhapi.Badges(json.RawMessage(badgesRaw))
	}
	return v
}

// T69 §6: the applied line carries title, employer, badges and resume.
func TestAppliedLineSegments(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(nil))
	s.Applied(false, "acc1", "fe31e03eff110666c30039ed1f4f5a59327851",
		vacancy(`{"badges":{"badge":[{"type":"employer-it-accreditation"}]}}`))
	out := buf.String()
	for _, want := range []string{
		"✓ Откликнулся:", "«Backend-разработчик (Go)»", "Corp LLC",
		"[аккредитованный ИТ]", "Таганрог", "150000–250000 RUR", "резюме «fe31e03e»",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("applied line %q misses %q", out, want)
		}
	}
}

// Empty segments (badges, area, salary) disappear silently.
func TestAppliedLineEmptySegmentsVanish(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(nil))
	s.Applied(false, "acc1", "hash1", hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"})
	out := buf.String()
	if strings.Contains(out, "[]") || strings.Contains(out, " ·  ") || strings.Contains(out, "резюме «»") {
		t.Fatalf("empty segments leaked: %q", out)
	}
	if !strings.Contains(out, "✓ Откликнулся: «Go Dev» — Corp LLC · резюме «hash1»") {
		t.Fatalf("unexpected line: %q", out)
	}
}

// Dry-run flips the verb to «Откликнулся бы».
func TestAppliedDryRunVerb(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(nil))
	s.Applied(true, "acc1", "hash1", hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"})
	if !strings.Contains(buf.String(), "✓ Откликнулся бы: «Go Dev»") {
		t.Fatalf("dry-run verb missing: %q", buf.String())
	}
	if strings.Contains(buf.String(), "Откликнулся:") {
		t.Fatalf("plain verb leaked into dry-run: %q", buf.String())
	}
}

// T69 §6: the skip modes gate the individual lines.
func TestSkipModes(t *testing.T) {
	routine := hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"}
	for _, tc := range []struct {
		modes string
		show  map[string]bool // reason → shown
	}{
		{"none", map[string]bool{"already_applied": false, "ai_needs_human": false}},
		{"all", map[string]bool{"already_applied": true, "ai_needs_human": true, "blacklisted": true}},
		{"interesting", map[string]bool{"ai_needs_human": true, "already_applied": false, "blacklisted": false}},
		{"aggregate", map[string]bool{"ai_needs_human": false, "already_applied": false}},
		{"interesting,aggregate", map[string]bool{"ai_needs_human": true, "already_applied": false}},
	} {
		s, buf := newTestSurface(t, false, testCfg(func(c *config.Console) { c.Skips = tc.modes }))
		for reason, want := range tc.show {
			buf.Reset()
			s.Skip("acc1", reason, routine)
			got := strings.Contains(buf.String(), "Пропущен")
			if got != want {
				t.Fatalf("modes %q reason %q: shown=%t, want %t (%q)", tc.modes, reason, got, want, buf.String())
			}
		}
	}
}

// T69 §6: skips_reasons is an allowlist on top of the mode; an unknown
// reason stays hidden.
func TestSkipsReasonsFilter(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(func(c *config.Console) {
		c.Skips = "all"
		c.SkipsReasons = []string{"ai_injection"}
	}))
	v := hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"}
	s.Skip("acc1", "ai_injection", v)
	if !strings.Contains(buf.String(), "ai_injection") {
		t.Fatalf("allowlisted reason hidden: %q", buf.String())
	}
	buf.Reset()
	s.Skip("acc1", "already_applied", v)
	if buf.Len() != 0 {
		t.Fatalf("non-allowlisted reason shown: %q", buf.String())
	}
}

// T69 §6: template_applied overrides the default wholesale.
func TestTemplateOverride(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(func(c *config.Console) {
		c.TemplateApplied = `MROW {{.VacancyTitle}} @ {{.Employer}}{{if .Badges}} b:{{.Badges}}{{end}}`
	}))
	s.Applied(false, "acc1", "hash1", vacancy(`{"badges":[{"type":"employer-hh-rating"}]}`))
	want := "MROW Backend-разработчик (Go) @ Corp LLC b:рейтинг hh\n"
	if buf.String() != want {
		t.Fatalf("override render = %q, want %q", buf.String(), want)
	}
}

// T69 §6: the digest aggregates per category, non-zero only, fixed order.
func TestDigestAggregation(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(nil))
	s.Digest(DigestLine{
		Applied: 2, Letters: 1, Skipped: 5,
		SkipBreakdown: SkipBreakdownByReason(map[string]int{
			"already_applied": 2, "score_below_threshold": 3, "employer_daily_cap": 1, "blacklisted": 4,
		}),
		Errors: 1,
	})
	want := "✓ Проход: откликов 2 · писем на проверку 1 · пропущено 5 (дедуп 2, ИИ 3, квота 1, прочее 4) · ошибок 1 · поднято 0\n"
	if buf.String() != want {
		t.Fatalf("digest = %q, want %q", buf.String(), want)
	}
	// Zero skips: the parens disappear.
	buf.Reset()
	s.Digest(DigestLine{Applied: 1})
	if !strings.Contains(buf.String(), "пропущено 0 ·") || strings.Contains(buf.String(), "()") {
		t.Fatalf("empty breakdown leaked: %q", buf.String())
	}
}

// T69 §6: TTY surfaces wrap lines in ANSI, non-TTY stays plain.
func TestColorGate(t *testing.T) {
	cfg := testCfg(nil)
	sColor, bufC := newTestSurface(t, true, cfg)
	sColor.Letter("acc1", hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"})
	if !strings.Contains(bufC.String(), "\033[33m? Письмо на проверку") {
		t.Fatalf("color surface lost ANSI: %q", bufC.String())
	}
	sPlain, bufP := newTestSurface(t, false, cfg)
	sPlain.Letter("acc1", hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"})
	if strings.Contains(bufP.String(), "\033[") {
		t.Fatalf("plain surface colored: %q", bufP.String())
	}
	// A buffer sink through New is never a terminal → plain even when allowed.
	bufN := &bytes.Buffer{}
	sN, err := New(bufN, true, cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sN.Error("acc1", hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"}, "boom")
	if strings.Contains(bufN.String(), "\033[") {
		t.Fatalf("non-TTY sink colored: %q", bufN.String())
	}
}

// T69 §6: the verdicts toggle gates the classification lines (cyan).
func TestVerdictsToggle(t *testing.T) {
	v := hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"}
	off, bufOff := newTestSurface(t, false, testCfg(nil))
	off.Verdict(v, 72, 60, true, "")
	if bufOff.Len() != 0 {
		t.Fatalf("verdicts off but rendered: %q", bufOff.String())
	}
	on, bufOn := newTestSurface(t, false, testCfg(func(c *config.Console) { c.Verdicts = true }))
	on.Verdict(v, 72, 60, true, "")
	if got, want := bufOn.String(), "Вердикт: «Go Dev» — score 72, порог 60 → отклик\n"; got != want {
		t.Fatalf("verdict = %q, want %q", got, want)
	}
	bufOn.Reset()
	on.Verdict(v, 45, 60, false, "score_below_threshold")
	if got, want := bufOn.String(), "Вердикт: «Go Dev» — score 45, порог 60 → скип (score_below_threshold)\n"; got != want {
		t.Fatalf("verdict = %q, want %q", got, want)
	}
}

// The lift line renders title-or-hash and the plan moment.
func TestLiftLine(t *testing.T) {
	s, buf := newTestSurface(t, false, testCfg(nil))
	s.Lift("acc1", "fe31e03eff110666", "", time.Date(2026, 9, 29, 22, 5, 0, 0, time.Local))
	if !strings.Contains(buf.String(), "↑ Поднято: «fe31e03e»") {
		t.Fatalf("lift line: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "следующий подъём ~") {
		t.Fatalf("lift line misses NextAt: %q", buf.String())
	}
	// Store snapshot title wins over the hash fallback.
	s2, buf2 := newTestSurface(t, false, testCfg(nil))
	s2.titles = func(_, _ string) string { return "Backend-разработчик" }
	s2.Lift("acc1", "hash1", "", time.Now())
	if !strings.Contains(buf2.String(), "«Backend-разработчик»") {
		t.Fatalf("title lookup ignored: %q", buf2.String())
	}
}

// SalaryText covers the range / bounded / empty cases.
func TestSalaryText(t *testing.T) {
	cases := []struct {
		s    *hhapi.Salary
		want string
	}{
		{nil, ""},
		{&hhapi.Salary{From: 150000, To: 250000, Currency: "RUR"}, "150000–250000 RUR"},
		{&hhapi.Salary{From: 100000}, "от 100000"},
		{&hhapi.Salary{To: 90000, Currency: "RUR"}, "до 90000 RUR"},
		{&hhapi.Salary{Currency: "RUR"}, ""},
	}
	for _, tc := range cases {
		if got := SalaryText(tc.s); got != tc.want {
			t.Fatalf("SalaryText(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// Toggles off silence their line types.
func TestTogglesOff(t *testing.T) {
	off := false
	s, buf := newTestSurface(t, false, testCfg(func(c *config.Console) {
		c.Applied, c.Errors, c.Letters, c.Lift = &off, &off, &off, &off
	}))
	v := hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"}
	s.Applied(false, "acc1", "hash1", v)
	s.Error("acc1", v, "boom")
	s.Letter("acc1", v)
	s.Lift("acc1", "hash1", "t", time.Now())
	if buf.Len() != 0 {
		t.Fatalf("toggles off but lines rendered: %q", buf.String())
	}
}

// A nil surface is silent everywhere.
func TestNilSurfaceSilent(_ *testing.T) {
	var s *Surface
	v := hhapi.Vacancy{ID: 1, Name: "Go Dev", EmployerName: "Corp LLC"}
	s.Applied(false, "acc1", "hash1", v)
	s.Skip("acc1", "ai_needs_human", v)
	s.Error("acc1", v, "boom")
	s.Letter("acc1", v)
	s.Verdict(v, 10, 5, false, "x")
	s.Lift("acc1", "hash1", "", time.Now())
	s.Digest(DigestLine{})
}

// formatNextAt: the full "02.01 15:04" local form (lifts cross midnight,
// a bare HH:MM would read as today); zero renders empty.
func TestFormatNextAt(t *testing.T) {
	at := time.Date(2026, 9, 24, 14, 15, 0, 0, time.Local)
	if got := formatNextAt(at); got != "24.09 14:15" {
		t.Fatalf("format = %q, want 24.09 14:15", got)
	}
	if got := formatNextAt(time.Time{}); got != "" {
		t.Fatalf("zero moment = %q", got)
	}
}
