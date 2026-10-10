// Package rejections (T123) is the rejection loop MVP: collect the
// discard-filtered negotiations, analyze the fresh ones with the
// reject_analysis AI task (vacancy card + chat transcript as data, the
// profile as the only fact source), store the verdicts and print the
// category report with the fix-plan. MVP policy: REPORT-ONLY — the owner
// decides on every change; nothing is auto-applied to the resume.
package rejections

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// APIClient is the hh surface the loop needs (a *hhapi.Client satisfies it).
type APIClient interface {
	NegotiationsDiscard(ctx context.Context, page int) ([]hhapi.Negotiation, error)
	ChatHistory(ctx context.Context, chatID string) ([]hhapi.ChatMessage, error)
	VacancyCard(ctx context.Context, id int64) (*hhapi.Vacancy, error)
}

// Options wires one Analyze run.
type Options struct {
	AccountID string
	API       APIClient
	Runner    ai.Runner // must have reject_analysis configured
	DB        *store.DB
	Profile   profile.Profile
	Log       *slog.Logger
	Out       io.Writer // console report
	DataDir   string    // md report destination directory
	Now       func() time.Time
	Days      int // analysis window (default 30)
	MaxPages  int // discard-page cap (default 20)
}

// Result is the run outcome for the command layer.
type Result struct {
	Seen            int // in-window structural refusals found
	Analyzed        int // stored this run
	SkippedExisting int // already in the store (idempotency gate)
	SkippedAI       int // needs_human verdicts — stored nowhere
	Guesses         int // stored with evidence_based=false
	ReportPath      string
}

// Analyze runs the loop: collect → analyze → store → report.
func Analyze(ctx context.Context, opt Options) (Result, error) {
	switch {
	case opt.API == nil:
		return Result{}, fmt.Errorf("rejections: api is nil")
	case opt.Runner == nil:
		return Result{}, fmt.Errorf("rejections: ИИ не настроен: добавьте [ai.tasks.reject_analysis] с model_profile")
	case opt.DB == nil:
		return Result{}, fmt.Errorf("rejections: db is nil")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Days <= 0 {
		opt.Days = 30
	}
	if opt.MaxPages <= 0 {
		opt.MaxPages = 20
	}
	window := opt.Now().AddDate(0, 0, -opt.Days)

	items, err := collectDiscard(ctx, opt, window)
	if err != nil {
		return Result{}, err
	}
	res := Result{Seen: len(items)}

	for _, neg := range items {
		if existing, gerr := opt.DB.RejectionsGet(ctx, opt.AccountID, neg.VacancyID); gerr != nil {
			return res, fmt.Errorf("rejections: lookup: %w", gerr)
		} else if existing != nil {
			res.SkippedExisting++
			continue
		}
		stored, err := analyzeOne(ctx, opt, neg)
		if err != nil {
			return res, fmt.Errorf("rejections: vacancy %d: %w", neg.VacancyID, err)
		}
		switch stored {
		case storedAnalyzed:
			res.Analyzed++
		case storedGuess:
			res.Analyzed++
			res.Guesses++
		case storedSkippedAI:
			res.SkippedAI++
		}
	}

	rows, err := opt.DB.RejectionsList(ctx, opt.AccountID)
	if err != nil {
		return res, fmt.Errorf("rejections: report source: %w", err)
	}
	res.ReportPath = writeMarkdownReport(opt.DataDir, rows, res, opt.Days, opt.Now())
	printConsoleReport(opt.Out, rows, res, opt.Days)
	return res, nil
}

// storedKind classifies one analysis outcome.
type storedKind int

const (
	storedAnalyzed  storedKind = iota // evidence-based verdict stored
	storedGuess                       // verdict stored, marked as a guess
	storedSkippedAI                   // needs_human verdict — stored nowhere
)

// collectDiscard pages the SSR negotiations page and keeps structural
// refusals inside the window. LIVE FACT (T123 probe): hh serves the mixed
// list despite filter=discard, so the State post-filter is the real gate;
// stop on an empty page, a fully-older page, or the page cap.
func collectDiscard(ctx context.Context, opt Options, window time.Time) ([]hhapi.Negotiation, error) {
	var out []hhapi.Negotiation
	for page := 1; page <= opt.MaxPages; page++ {
		negs, err := opt.API.NegotiationsDiscard(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("discard page %d: %w", page, err)
		}
		if len(negs) == 0 {
			break
		}
		fresh := 0
		for _, n := range negs {
			if n.UpdatedAt.Before(window) {
				continue
			}
			fresh++
			if hhapi.IsStructuralRefusal(n.State) {
				out = append(out, n)
			}
		}
		if fresh == 0 { // pages run newest-first; older pages hold nothing fresh
			break
		}
	}
	return out, nil
}

// analyzeOne produces and stores one verdict. The transcript's employer
// messages are the evidence: no employer words → the row is marked a guess
// (kept, never dropped). A needs_human verdict (grounding/injection) is
// stored nowhere — the donor lesson: template causes would be guesses.
func analyzeOne(ctx context.Context, opt Options, neg hhapi.Negotiation) (storedKind, error) {
	card, cardErr := opt.API.VacancyCard(ctx, neg.VacancyID)
	descMissing := false
	if cardErr != nil {
		// Archived/removed cards are expected for old rejections; the
		// negotiation item's names keep the analysis possible.
		opt.Log.Warn("rejections: vacancy card unavailable; analyzing from the negotiation item",
			"vacancy", neg.VacancyID, "err", cardErr)
		card = &hhapi.Vacancy{ID: neg.VacancyID, Name: neg.VacancyName, EmployerName: neg.EmployerName}
		descMissing = true
	}

	var transcript []transcriptMsg
	employerMsgs := 0
	if neg.ChatID != "" {
		msgs, herr := opt.API.ChatHistory(ctx, neg.ChatID)
		if herr != nil {
			opt.Log.Warn("rejections: chat history unavailable; treating as a guess",
				"chat", neg.ChatID, "err", herr)
		} else {
			transcript, employerMsgs = cleanTranscript(msgs)
		}
	}

	raw, _, err := opt.Runner.Run(ctx, opt.AccountID, "negotiation", neg.ID, "",
		ai.TaskRejectionAnalysis, rejectInput{
			ResumeDigest:       opt.Profile.ResumeDigest,
			ProfileText:        opt.Profile.ProfileText,
			Vacancy:            slimCard(card),
			VacancyDescMissing: descMissing,
			Transcript:         transcript,
			EmployerMessages:   employerMsgs,
		})
	if err != nil {
		return storedSkippedAI, err
	}
	var v struct {
		RootCause        string   `json:"root_cause"`
		Critique         string   `json:"critique"`
		ImprovedLetter   *string  `json:"improved_letter"`
		MissingSkills    []string `json:"missing_skills"`
		AboutMeRec       string   `json:"about_me_rec"`
		Takeaway         string   `json:"takeaway"`
		NeedsHuman       bool     `json:"needs_human"`
		NeedsHumanReason string   `json:"needs_human_reason"`
		Injection        bool     `json:"injection_suspected"`
	}
	if jerr := json.Unmarshal(raw, &v); jerr != nil {
		return storedSkippedAI, jerr
	}
	if v.NeedsHuman || v.Injection {
		opt.Log.Warn("rejections: verdict needs a human; not stored",
			"vacancy", neg.VacancyID, "reason", v.NeedsHumanReason)
		return storedSkippedAI, nil
	}
	letter := ""
	if v.ImprovedLetter != nil {
		letter = strings.TrimSpace(*v.ImprovedLetter)
	}
	row := &store.Rejection{
		AccountID:      opt.AccountID,
		VacancyID:      neg.VacancyID,
		VacancyName:    firstNonEmpty(card.Name, neg.VacancyName),
		EmployerName:   firstNonEmpty(card.EmployerName, neg.EmployerName),
		NegotiationID:  neg.ID,
		RejectedAt:     neg.UpdatedAt,
		RootCause:      strings.TrimSpace(v.RootCause),
		EvidenceBased:  employerMsgs > 0,
		ImprovedLetter: letter,
		MissingSkills:  v.MissingSkills,
		Takeaway:       strings.TrimSpace(v.Takeaway),
	}
	if err := opt.DB.RejectionsPut(ctx, row); err != nil {
		return storedSkippedAI, err
	}
	if row.EvidenceBased {
		return storedAnalyzed, nil
	}
	return storedGuess, nil
}

// slimCard drops the raw payload (token burn, no prompt value).
func slimCard(v *hhapi.Vacancy) hhapi.Vacancy {
	c := *v
	c.Raw = nil
	if c.Description != "" {
		c.Description = truncateRunes(htmlToText(c.Description), maxDescriptionRunes)
	}
	return c
}

// cleanTranscript keeps the last messages, drops empty lines and caps each
// text; it counts the employer's own (in) messages — the evidence signal.
func cleanTranscript(msgs []hhapi.ChatMessage) ([]transcriptMsg, int) {
	const (
		maxMessages   = 40
		maxMessageRun = 800
	)
	if len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	out := make([]transcriptMsg, 0, len(msgs))
	employer := 0
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		if m.Direction == "in" {
			employer++
		}
		out = append(out, transcriptMsg{
			Direction: m.Direction,
			Author:    m.Author,
			Text:      truncateRunes(text, maxMessageRun),
			SentAt:    m.SentAt.Format(time.RFC3339),
		})
	}
	return out, employer
}

type transcriptMsg struct {
	Direction string `json:"direction"`
	Author    string `json:"author,omitempty"`
	Text      string `json:"text"`
	SentAt    string `json:"sent_at,omitempty"`
}

type rejectInput struct {
	ResumeDigest       string          `json:"resume_digest,omitempty"`
	ProfileText        string          `json:"profile_text,omitempty"`
	Vacancy            hhapi.Vacancy   `json:"vacancy"`
	VacancyDescMissing bool            `json:"vacancy_description_missing,omitempty"`
	Transcript         []transcriptMsg `json:"transcript"`
	EmployerMessages   int             `json:"employer_messages"`
}

// --- description text -------------------------------------------------------

const maxDescriptionRunes = 12000

const truncatedMark = "\n…[обрезано]"

var (
	scriptBlocksRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blockTagsRe    = regexp.MustCompile(`(?i)</?(p|div|li|tr|h[1-6])[^>]*>|<br\s*/?>`)
	anyTagRe       = regexp.MustCompile(`<[^>]*>`)
	inlineSpacesRe = regexp.MustCompile(`[^\S\n]+`)
	blankLinesRe   = regexp.MustCompile(`\n{3,}`)
)

// htmlToText renders an hh HTML description as plain text (same contract as
// the cover-letter path), capped at maxDescriptionRunes.
func htmlToText(s string) string {
	s = scriptBlocksRe.ReplaceAllString(s, "")
	s = blockTagsRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = inlineSpacesRe.ReplaceAllString(s, " ")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return truncateRunes(strings.TrimSpace(strings.Join(lines, "\n")), maxDescriptionRunes)
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + truncatedMark
}

// --- report -----------------------------------------------------------------

// causeCategory buckets a root cause by keywords (donor
// classify_rejection_reason style, conservative word list).
type causeCategory struct {
	key, label string
	keywords   []string
}

var causeCategories = []causeCategory{
	{"salary", "зарплата", []string{"зарплат", "оклад", "бюджет", "вилк", "₽", "руб", "доход"}},
	{"location", "локация/формат", []string{"город", "офис", "удалён", "удален", "релокаци", "переезд", "гибрид", "смену", "график"}},
	{"experience", "грейд/стаж", []string{"грейд", "стаж", "лет опыт", "года опыт", "junior", "senior", "middle", "мидл", "сеньор", "джун"}},
	{"skills", "навыки/стек", []string{"стек", "навык", "технолог", "знани", "просят", "требует", "нет в резюме"}},
	{"application_quality", "качество отклика", []string{"письм", "сопровод", "отклик", "оформлен"}},
	{"competition", "выбрали другого", []string{"другого кандидат", "выбрали другог", "продолжил с друг", "конкур"}},
}

// classifyCause maps a root cause to its category label (first match wins).
func classifyCause(cause string) string {
	lc := strings.ToLower(cause)
	for _, c := range causeCategories {
		for _, kw := range c.keywords {
			if strings.Contains(lc, kw) {
				return c.label
			}
		}
	}
	return "другое"
}

type causeStat struct {
	label     string
	count     int
	evidenced int
	example   string
}

func aggregateCauses(rows []store.Rejection) []causeStat {
	byLabel := map[string]*causeStat{}
	var order []string
	for _, r := range rows {
		label := classifyCause(r.RootCause)
		st, ok := byLabel[label]
		if !ok {
			st = &causeStat{label: label, example: r.RootCause}
			byLabel[label] = st
			order = append(order, label)
		}
		st.count++
		if r.EvidenceBased {
			st.evidenced++
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return byLabel[order[i]].count > byLabel[order[j]].count
	})
	out := make([]causeStat, 0, len(order))
	for _, label := range order {
		out = append(out, *byLabel[label])
	}
	return out
}

// skillCount groups missing skills case-insensitively keeping the most
// common original spelling (donor skill_forms lesson).
func skillCount(rows []store.Rejection) []string {
	counts := map[string]int{}
	display := map[string]string{}
	for _, r := range rows {
		for _, s := range r.MissingSkills {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			norm := strings.ToLower(s)
			counts[norm]++
			if _, ok := display[norm]; !ok || len(s) < len(display[norm]) {
				display[norm] = s
			}
		}
	}
	type skill struct {
		norm, name string
		count      int
	}
	var list []skill
	for norm, count := range counts {
		list = append(list, skill{norm: norm, name: display[norm], count: count})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].name < list[j].name
	})
	const top = 10
	out := make([]string, 0, len(list))
	for i, s := range list {
		if i == top {
			break
		}
		out = append(out, fmt.Sprintf("%s (%d)", s.name, s.count))
	}
	return out
}

// printConsoleReport writes the human report (T49: plain Russian text).
func printConsoleReport(w io.Writer, rows []store.Rejection, res Result, days int) {
	if res.Seen == 0 && res.Analyzed == 0 && res.SkippedExisting == 0 && len(rows) == 0 {
		_, _ = fmt.Fprintf(w, "✓ Разбор отказов: отказов в окне %d дн. не найдено\n", days)
		return
	}
	_, _ = fmt.Fprintf(w, "✓ Разбор отказов: в окне %d дн. найдено %d · разобрано %d · уже в базе %d · отклонено ИИ %d · догадок %d\n",
		days, res.Seen, res.Analyzed, res.SkippedExisting, res.SkippedAI, res.Guesses)

	stats := aggregateCauses(rows)
	evidence := 0
	for _, r := range rows {
		if r.EvidenceBased {
			evidence++
		}
	}
	if evidence > 0 {
		_, _ = fmt.Fprintln(w, "Причины отказов (подтверждены работодателем):")
		for _, s := range stats {
			if s.evidenced == 0 {
				continue
			}
			_, _ = fmt.Fprintf(w, "  · %s — %d (%d%%)\n", s.label, s.evidenced, s.evidenced*100/evidence)
		}
	}
	if res.Guesses > 0 {
		_, _ = fmt.Fprintln(w, "Догадки (нет слов работодателя — оценка, не факт):")
		for _, s := range stats {
			if s.evidenced > 0 || s.count == s.evidenced {
				continue
			}
			_, _ = fmt.Fprintf(w, "  · %s — %d\n", s.label, s.count-s.evidenced)
		}
	}
	_, _ = fmt.Fprintln(w, "Fix-plan (решение за владельцем — авто-правок нет):")
	if skills := skillCount(rows); len(skills) > 0 {
		_, _ = fmt.Fprintf(w, "  · навыки (top): %s\n", strings.Join(skills, ", "))
	} else {
		_, _ = fmt.Fprintln(w, "  · навыки: не определены (нет подтверждённых данных)")
	}
}

// writeMarkdownReport saves the fuller report; returns the path ("" when
// DataDir is unset).
func writeMarkdownReport(dataDir string, rows []store.Rejection, res Result, days int, now time.Time) string {
	if dataDir == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Разбор отказов — %s (окно %d дн.)\n\n", now.Format("02.01.2006 15:04"), days)
	fmt.Fprintf(&b, "Найдено %d · разобрано %d · уже в базе %d · отклонено ИИ %d · догадок %d\n\n",
		res.Seen, res.Analyzed, res.SkippedExisting, res.SkippedAI, res.Guesses)

	stats := aggregateCauses(rows)
	evidence := 0
	for _, r := range rows {
		if r.EvidenceBased {
			evidence++
		}
	}
	b.WriteString("## Причины отказов (подтверждены работодателем)\n\n")
	shown := false
	for _, s := range stats {
		if s.evidenced == 0 {
			continue
		}
		shown = true
		fmt.Fprintf(&b, "- %s — %d (%d%%)\n  - пример: «%s»\n", s.label, s.evidenced, s.evidenced*100/evidence, clipLine(s.example, 300))
	}
	if !shown {
		b.WriteString("- нет подтверждённых причин\n")
	}
	if res.Guesses > 0 {
		b.WriteString("\n## Догадки (без слов работодателя — оценка, не факт)\n\n")
		for _, r := range rows {
			if r.EvidenceBased {
				continue
			}
			fmt.Fprintf(&b, "- «%s»\n", clipLine(r.RootCause, 300))
		}
	}

	b.WriteString("\n## Детально\n\n")
	if len(rows) > 0 {
		b.WriteString("| Вакансия | Работодатель | Дата отказа | Причина | Доказательство | Навыки |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		for _, r := range rows {
			ev := "догадка"
			if r.EvidenceBased {
				ev = "слова работодателя"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
				mdCell(r.VacancyName, 60), mdCell(r.EmployerName, 40),
				r.RejectedAt.Format("02.01.2006"), mdCell(r.RootCause, 200), ev,
				mdCell(strings.Join(r.MissingSkills, ", "), 120))
		}
	} else {
		b.WriteString("Разобранных отказов в базе нет.\n")
	}

	b.WriteString("\n## Fix-plan (решение за владельцем — авто-правок нет)\n\n")
	if skills := skillCount(rows); len(skills) > 0 {
		fmt.Fprintf(&b, "- навыки (top): %s\n", strings.Join(skills, ", "))
	} else {
		b.WriteString("- навыки: не определены (нет подтверждённых данных)\n")
	}
	for _, s := range stats {
		if s.evidenced > 0 {
			fmt.Fprintf(&b, "- категория «%s»: %d подтверждённых (%d%%)\n", s.label, s.evidenced, s.evidenced*100/max(1, evidence))
		}
	}

	path := filepath.Join(dataDir, "rejections-report.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return ""
	}
	return path
}

func mdCell(s string, limit int) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return clipLine(s, limit)
}

func clipLine(s string, limit int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
