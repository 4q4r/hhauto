// Package console renders the daemon's human-readable console surface
// (T69): the per-event vacancy lines (applied, skip, error, letter), the
// AI classification verdicts, the autolift success lines and the
// end-of-pass digest. Every line goes through a Go text/template — the
// built-in default or the [console].template_* override — and the colors
// follow the T53 vocabulary (green success, red error, yellow attention,
// dim secondary, cyan info) gated by TTY && !NO_COLOR && TERM!=dumb.
// The surface is presentation only: ledger rows, metrics and webhooks are
// emitted elsewhere and are never affected by these toggles.
package console

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"
	"time"

	"golang.org/x/term"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
)

// ANSI SGR codes (T53 vocabulary; same codes as the cli style and the
// logging console handler).
const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiCyan   = "\033[36m"
	ansiDim    = "\033[2m"
)

// Default templates (T69). {{if}} guards make empty segments disappear
// silently; the applied verb flips to «Откликнулся бы» in dry-run and the
// digest parens vanish when no vacancy was skipped.
const (
	defaultAppliedTmpl = `✓ {{if .Dry}}Откликнулся бы{{else}}Откликнулся{{end}}{{if .Note}} ({{.Note}}){{end}}: «{{.VacancyTitle}}» — {{.Employer}}{{if .Badges}} [{{.Badges}}]{{end}}{{if .Area}} · {{.Area}}{{end}}{{if .Salary}} · {{.Salary}}{{end}} · резюме «{{.ResumeTitle}}»`
	defaultSkipTmpl    = `· Пропущен: «{{.VacancyTitle}}» — {{.Employer}}: {{.Reason}}`
	defaultErrorTmpl   = `✗ Ошибка отклика: «{{.VacancyTitle}}» — {{.Employer}}: {{.Reason}}`
	defaultLetterTmpl  = `? Письмо на проверку: «{{.VacancyTitle}}» — {{.Employer}}`
	defaultCaptchaTmpl = `⚑ Капча: «{{.VacancyTitle}}» — {{.Employer}} · решателя не настроено, вакансия пропущена до следующего цикла`
	defaultDigestTmpl  = `✓ Проход: откликов {{.Applied}} · писем на проверку {{.Letters}} · пропущено {{.Skipped}}{{if .SkipBreakdown}} ({{.SkipBreakdown}}){{end}} · ошибок {{.Errors}} · поднято {{.Lifts}}`
	defaultLiftTmpl    = `↑ Поднято: «{{.ResumeTitle}}» · следующий подъём ~{{.NextAt}}`
)

// VacancyLine is the template data for the per-vacancy lines (applied,
// skip, error, letter). Empty fields disappear through the default
// templates' {{if}} guards.
type VacancyLine struct {
	Dry          bool
	VacancyTitle string
	Employer     string
	Badges       string // labels joined with ", "; empty when the vacancy carries none
	Area         string
	Salary       string
	ResumeTitle  string // store snapshot title, short hash as the fallback
	Reason       string
	Note         string // applied-line suffix, e.g. "gray 50" (T75 gray-band origin)
}

// DigestLine is the template data for the end-of-pass digest. Skipped and
// SkipBreakdown cover the same skips: the total and the per-category view
// (dedup / AI / quota / other, non-zero categories only).
type DigestLine struct {
	Applied       int
	Skipped       int
	Letters       int
	Errors        int
	Lifts         int
	SkipBreakdown string
}

// LiftLine is the template data for the autolift success line.
type LiftLine struct {
	ResumeTitle string
	NextAt      string // pre-formatted plan moment (local time)
}

// Surface renders console lines. Methods on a nil *Surface are silent, so
// unwired tests and disabled paths degrade to no output.
type Surface struct {
	w       io.Writer
	color   bool
	cfg     config.Console
	modes   map[string]bool
	reasons map[string]bool
	titles  func(accountID, hash string) string
	tmpl    map[string]*template.Template
}

// templateSpec pairs a template name with its built-in default.
type templateSpec struct {
	name          string
	def           string
	overrideField func(c config.Console) string
}

// templateSpecs lists every line template in one deterministic place.
var templateSpecs = []templateSpec{
	{"applied", defaultAppliedTmpl, func(c config.Console) string { return c.TemplateApplied }},
	{"skip", defaultSkipTmpl, func(c config.Console) string { return c.TemplateSkip }},
	{"error", defaultErrorTmpl, func(c config.Console) string { return c.TemplateError }},
	{"captcha", defaultCaptchaTmpl, func(config.Console) string { return "" }}, // T88: fixed format — no [console] override (the Verdict precedent)
	{"letter", defaultLetterTmpl, func(c config.Console) string { return c.TemplateLetter }},
	{"digest", defaultDigestTmpl, func(c config.Console) string { return c.TemplateDigest }},
	{"lift", defaultLiftTmpl, func(c config.Console) string { return c.TemplateLift }},
}

// New builds the console surface for the process stderr (or any writer).
// Colors follow the T53 gate: a terminal sink without --no-color, NO_COLOR
// or TERM=dumb. Malformed overrides are rejected by config.Validate before
// the daemon starts; New still surfaces parse errors for direct callers.
func New(w io.Writer, colorAllowed bool, cfg config.Console, titles func(accountID, hash string) string) (*Surface, error) {
	color := false
	if f, ok := w.(*os.File); ok && colorAllowed && term.IsTerminal(int(f.Fd())) &&
		os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" {
		color = true
	}
	return newSurface(w, color, cfg, titles)
}

// newSurface builds a surface with an explicit color switch (the test
// seam; New derives it from the T53 gate).
func newSurface(w io.Writer, color bool, cfg config.Console, titles func(accountID, hash string) string) (*Surface, error) {
	s := &Surface{
		w: w, color: color, cfg: cfg,
		modes:   cfg.SkipModes(),
		reasons: map[string]bool{},
		titles:  titles,
		tmpl:    make(map[string]*template.Template, len(templateSpecs)),
	}
	for _, r := range cfg.SkipsReasons {
		s.reasons[r] = true
	}
	for _, spec := range templateSpecs {
		body := spec.def
		if ov := spec.overrideField(cfg); ov != "" {
			body = ov
		}
		t, err := template.New(spec.name).Parse(body)
		if err != nil {
			return nil, fmt.Errorf("console: template %s: %w", spec.name, err)
		}
		s.tmpl[spec.name] = t
	}
	return s, nil
}

// boolOn reads a default-true *bool toggle (R1: nil = on).
func boolOn(b *bool) bool { return b == nil || *b }

// render executes one line template and wraps the result in the line's
// semantic color. A template execution failure (an override referencing a
// wrong field parses fine but fails here) writes a plain diagnostic line
// instead of silently dropping the event.
func (s *Surface) render(name, color string, data any) {
	if s == nil {
		return
	}
	var b strings.Builder
	if err := s.tmpl[name].Execute(&b, data); err != nil {
		// Best-effort console: a closed pipe must not turn the daemon
		// into a failure; the diagnostic still lands on the writer.
		_, _ = fmt.Fprintf(s.w, "console: ошибка шаблона %s: %v\n", name, err)
		return
	}
	line := b.String()
	if s.color && color != "" {
		line = color + line + ansiReset
	}
	_, _ = io.WriteString(s.w, line+"\n")
}

// Applied renders the apply success line (green): «Откликнулся» or, in
// dry-run, «Откликнулся бы». An empty note renders no suffix; the gray-band
// origin passes "gray N" (T75).
func (s *Surface) Applied(dry bool, accountID, resumeHash string, v hhapi.Vacancy, note string) {
	if s == nil || !boolOn(s.cfg.Applied) {
		return
	}
	s.render("applied", ansiGreen, s.vacancyLine(accountID, resumeHash, v, VacancyLine{Dry: dry, Note: note}))
}

// Skip renders one individual skip line (dim) when the configured mode and
// the optional reason allowlist let it through. The mode and the allowlist
// key on the raw machine reason; the rendered line uses the human label
// when one exists (T88: captcha_exhausted → «капча (лимит прохода)»).
func (s *Surface) Skip(accountID, reason string, v hhapi.Vacancy) {
	if s == nil || !s.skipShown(reason) {
		return
	}
	s.render("skip", ansiDim, s.vacancyLine(accountID, "", v, VacancyLine{Reason: skipLabel(reason)}))
}

// Error renders the apply failure line (red).
func (s *Surface) Error(accountID string, v hhapi.Vacancy, reason string) {
	if s == nil || !boolOn(s.cfg.Errors) {
		return
	}
	s.render("error", ansiRed, s.vacancyLine(accountID, "", v, VacancyLine{Reason: reason}))
}

// Captcha renders the captcha-challenge line (yellow, T88): instead of the
// red error, a yellow attention line — the vacancy is skipped to the next
// cycle. The method takes no reason, so the raw challenge payload
// (captchaState JSON, HTTP details) cannot reach the console; it stays in
// the file log and the ledger row. Gated by the errors toggle — the line
// replaces what used to be the error line.
func (s *Surface) Captcha(accountID string, v hhapi.Vacancy) {
	if s == nil || !boolOn(s.cfg.Errors) {
		return
	}
	s.render("captcha", ansiYellow, s.vacancyLine(accountID, "", v, VacancyLine{}))
}

// CaptchaBreaker renders the captcha circuit-breaker banner (yellow, T88):
// one line per trip event (the breaker is day-scoped, so once a day per
// account) — the pass's remaining vacancies skip to the next cycle.
func (s *Surface) CaptchaBreaker() {
	if s == nil || !boolOn(s.cfg.Errors) {
		return
	}
	s.fixed(ansiYellow, "⚑ Капча-брейкер: hh начал капчить, решателя нет — остальные вакансии прохода пропущены до следующего цикла")
}

// Letter renders the needs-human cover letter line (yellow).
func (s *Surface) Letter(accountID string, v hhapi.Vacancy) {
	if s == nil || !boolOn(s.cfg.Letters) {
		return
	}
	s.render("letter", ansiYellow, s.vacancyLine(accountID, "", v, VacancyLine{}))
}

// Verdict renders one AI classification verdict line (cyan, fixed format —
// no template override for verdicts).
func (s *Surface) Verdict(v hhapi.Vacancy, score, threshold int, pass bool, reason string) {
	if s == nil || !s.cfg.Verdicts {
		return
	}
	outcome := "отклик"
	if !pass {
		outcome = fmt.Sprintf("скип (%s)", reason)
	}
	line := fmt.Sprintf("Вердикт: «%s» — score %d, порог %d → %s", v.Name, score, threshold, outcome)
	if s.color {
		line = ansiCyan + line + ansiReset
	}
	_, _ = io.WriteString(s.w, line+"\n")
}

// Lift renders the autolift success line (green): the resume title (short
// hash fallback) and the next lift moment from the local plan.
func (s *Surface) Lift(accountID, hash, title string, nextAt time.Time) {
	if s == nil || !boolOn(s.cfg.Lift) {
		return
	}
	if title == "" && s.titles != nil {
		title = s.titles(accountID, hash)
	}
	if title == "" {
		title = shortHash(hash)
	}
	s.render("lift", ansiGreen, LiftLine{ResumeTitle: title, NextAt: formatNextAt(nextAt)})
}

// Digest renders the end-of-pass digest (green).
func (s *Surface) Digest(d DigestLine) {
	s.render("digest", ansiGreen, d)
}

// DaemonStart renders the startup banner (green): one fixed line, the
// daemon's whole contract at a glance (T76 — replaces the English slog
// banner; one banner per event). Fixed format — daemon lifecycle is not
// vacancy content, so no [console] template override (the Verdict
// precedent).
func (s *Surface) DaemonStart() {
	s.fixed(ansiGreen, "✓ Демон запущен: отклики раз в 24ч · первый проход сейчас · подъёмы по окнам")
}

// CycleDone renders the pass-completion lifecycle line (dim). The per-run
// counters sit in the T69 digest lines printed during the pass; this line
// closes the cycle without repeating them.
func (s *Surface) CycleDone() {
	s.fixed(ansiDim, "· Проход завершён")
}

// Sleeping renders the pre-sleep line (dim): until when the daemon sleeps
// and which event it waits for — the apply cadence («отклики») or a
// nearer maintenance wake («подъём»).
func (s *Surface) Sleeping(until time.Time, maintenance bool) {
	nearest := "отклики"
	if maintenance {
		nearest = "подъём"
	}
	s.fixed(ansiDim, fmt.Sprintf("· Сплю до %s (ближайшее: %s)", until.Format("15:04"), nearest))
}

// fixed renders one fixed-format lifecycle line in its semantic color
// (the Verdict rendering pattern).
func (s *Surface) fixed(color, line string) {
	if s == nil {
		return
	}
	if s.color && color != "" {
		line = color + line + ansiReset
	}
	_, _ = io.WriteString(s.w, line+"\n")
}

// vacancyLine builds the template data: vacancy facts, badge labels, the
// salary text and the resume title (snapshot lookup, short hash fallback).
func (s *Surface) vacancyLine(accountID, resumeHash string, v hhapi.Vacancy, line VacancyLine) VacancyLine {
	line.VacancyTitle = v.Name
	line.Employer = v.EmployerName
	line.Badges = strings.Join(v.Badges, ", ")
	line.Area = v.Area
	line.Salary = SalaryText(v.Salary)
	if resumeHash != "" {
		title := ""
		if s.titles != nil {
			title = s.titles(accountID, resumeHash)
		}
		if title == "" {
			title = shortHash(resumeHash)
		}
		line.ResumeTitle = title
	}
	return line
}

// skipShown applies the [console].skips mode and the skips_reasons
// allowlist: aggregate never renders individual lines, interesting renders
// only the human-attention reasons, all renders everything; the allowlist
// narrows the result further.
func (s *Surface) skipShown(reason string) bool {
	switch {
	case s.modes["none"]:
		return false
	case s.modes["all"]:
	case s.modes["interesting"] && interestingReasons[reason]:
	default:
		return false
	}
	if len(s.reasons) > 0 && !s.reasons[reason] {
		return false
	}
	return true
}

// skipLabels translates machine skip reasons onto human Russian labels at
// the render point; reasons without an entry render verbatim.
var skipLabels = map[string]string{
	"captcha_exhausted": "капча (лимит прохода)",
}

// skipLabel resolves the human label for a skip reason.
func skipLabel(reason string) string {
	if label, ok := skipLabels[reason]; ok {
		return label
	}
	return reason
}

// interestingReasons lists the skip reasons worth an individual line:
// human attention or a structural surprise. The routine machinery
// (filters, dedup, quotas, threshold) counts in the digest only.
var interestingReasons = map[string]bool{
	"ai_needs_human":      true,
	"ai_injection":        true,
	"inconsistencies":     true,
	"letter_links":        true,
	"letter_unavailable":  true,
	"popup_failed":        true,
	"vacancy_card_failed": true,
	"intent_error":        true,
	"hh_limit":            true,
	"captcha_exhausted":   true,
}

// skipCategory maps a skip reason onto its digest breakdown category
// (T69: dedup / AI / quota / other).
func skipCategory(reason string) string {
	switch reason {
	case "already_applied", "dup_batch", "native_autoresponse",
		"intent_pending", "intent_done", "reapply_touched", "reapply_recent_rejection":
		return "дедуп"
	case "score_below_threshold", "ai_needs_human", "ai_injection", "ai_error":
		return "ИИ"
	case "run_quota", "account_cap", "employer_daily_cap", "hh_limit":
		return "квота"
	default:
		return "прочее"
	}
}

// SkipBreakdownByReason renders the digest breakdown from the pass's
// per-reason skip counts: reasons map onto the T69 categories (dedup /
// AI / quota / other) and the non-zero categories render in a fixed
// order, joined with ", ".
func SkipBreakdownByReason(skips map[string]int) string {
	byCat := map[string]int{}
	for reason, n := range skips {
		byCat[skipCategory(reason)] += n
	}
	var parts []string
	for _, cat := range []string{"дедуп", "ИИ", "квота", "прочее"} {
		if n := byCat[cat]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", cat, n))
		}
	}
	return strings.Join(parts, ", ")
}

// formatNextAt renders a plan moment for the lift line: the full local
// "02.01 15:04" form — lifts cross midnight (4h spacing), so a bare HH:MM
// would read as today.
func formatNextAt(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format("02.01 15:04")
}

// shortHash is the resume-title fallback: the first 8 hash characters.
func shortHash(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// SalaryText renders the compensation segment: «150000–250000 RUR»,
// «от 150000 RUR» or «до 250000 RUR»; empty when the search item carries
// no compensation.
func SalaryText(s *hhapi.Salary) string {
	if s == nil {
		return ""
	}
	var text string
	switch {
	case s.From > 0 && s.To > 0:
		text = fmt.Sprintf("%d–%d", s.From, s.To)
	case s.From > 0:
		text = fmt.Sprintf("от %d", s.From)
	case s.To > 0:
		text = fmt.Sprintf("до %d", s.To)
	default:
		return ""
	}
	if s.Currency != "" {
		text += " " + s.Currency
	}
	return text
}
