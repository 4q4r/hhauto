package profile

// BuildResumeDigest renders one hh.ru resume snapshot (the raw JSON stored
// by the write-through) as a compact, deterministic multi-line text block.
//
// Field shapes follow the documented hh.ru resume format (github.com/hhru/api:
// /resumes/mine items and the full resume view). Parsing is deliberately
// tolerant: absent fields are simply omitted, and both the list/plural
// variants ("employments", "schedules", "language") and the singular ones
// ("employment", "schedule", "languages"), as well as "skill_set" (strings)
// and "key_skills" ([{name}]), are accepted.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// maxDigestLine is the per-line clip (in runes) for free-text resume fields
// (professional skills, experience descriptions) so the digest stays compact.
const maxDigestLine = 300

type nameRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type salaryRef struct {
	Amount   json.Number `json:"amount"`
	Currency string      `json:"currency"`
}

type totalExpRef struct {
	Months int `json:"months"`
}

type expRef struct {
	Position    *string  `json:"position"`
	Company     *string  `json:"company"`
	Start       *string  `json:"start"`
	End         *string  `json:"end"`
	Area        *nameRef `json:"area"`
	Description *string  `json:"description"`
}

type langRef struct {
	Name  string   `json:"name"`
	Level *nameRef `json:"level"`
}

type relocationRef struct {
	Type *nameRef  `json:"type"`
	Area []nameRef `json:"area"`
}

type rawResume struct {
	Title        *string        `json:"title"`
	Status       *nameRef       `json:"status"`
	Area         *nameRef       `json:"area"`
	Salary       *salaryRef     `json:"salary"`
	TotalExp     *totalExpRef   `json:"total_experience"`
	Experience   []expRef       `json:"experience"`
	Skills       *string        `json:"skills"`
	SkillSet     []string       `json:"skill_set"`
	KeySkills    []nameRef      `json:"key_skills"`
	Employments  []nameRef      `json:"employments"`
	Employment   *nameRef       `json:"employment"`
	Schedules    []nameRef      `json:"schedules"`
	Schedule     *nameRef       `json:"schedule"`
	Language     []langRef      `json:"language"`
	Languages    []langRef      `json:"languages"`
	Relocation   *relocationRef `json:"relocation"`
	BusinessTrip *nameRef       `json:"business_trip_readiness"`
}

// BuildResumeDigest converts raw hh resume JSON into a deterministic text
// digest. Empty/nil raw is an error; a valid JSON object without any known
// field yields an empty digest with no error.
func BuildResumeDigest(raw json.RawMessage) (string, error) {
	if len(trimSpaceBytes(raw)) == 0 {
		return "", errors.New("profile: resume raw is empty")
	}
	var r rawResume
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("profile: parse resume raw: %w", err)
	}

	var b strings.Builder
	title := ""
	if r.Title != nil {
		title = strings.TrimSpace(*r.Title)
	}
	if title != "" || (r.Status != nil && r.Status.Name != "") {
		b.WriteString("Резюме")
		if title != "" {
			b.WriteString(" «" + title + "»")
		}
		if r.Status != nil && r.Status.Name != "" {
			b.WriteString(" (" + r.Status.Name + ")")
		}
		b.WriteString("\n")
	}
	if r.Area != nil && r.Area.Name != "" {
		fmt.Fprintf(&b, "Регион: %s\n", r.Area.Name)
	}
	if r.Salary != nil && r.Salary.Amount != "" {
		line := "Зарплата: " + r.Salary.Amount.String()
		if r.Salary.Currency != "" {
			line += " " + r.Salary.Currency
		}
		b.WriteString(line + "\n")
	}
	if r.TotalExp != nil && r.TotalExp.Months > 0 {
		fmt.Fprintf(&b, "Общий опыт: %s\n", formatMonths(r.TotalExp.Months))
	}
	if names := nameList(r.Employments, r.Employment); len(names) > 0 {
		b.WriteString("Занятость: " + strings.Join(names, ", ") + "\n")
	}
	if names := nameList(r.Schedules, r.Schedule); len(names) > 0 {
		b.WriteString("График: " + strings.Join(names, ", ") + "\n")
	}
	if r.Relocation != nil && r.Relocation.Type != nil && r.Relocation.Type.Name != "" {
		line := "Релокация: " + r.Relocation.Type.Name
		if names := refNames(r.Relocation.Area); len(names) > 0 {
			line += " (" + strings.Join(names, ", ") + ")"
		}
		b.WriteString(line + "\n")
	}
	if r.BusinessTrip != nil && r.BusinessTrip.Name != "" {
		fmt.Fprintf(&b, "Командировки: %s\n", r.BusinessTrip.Name)
	}
	if skills := skillsList(r); len(skills) > 0 {
		b.WriteString("Ключевые навыки: " + strings.Join(skills, ", ") + "\n")
	}
	if r.Skills != nil {
		if s := clipLine(oneLine(*r.Skills), maxDigestLine); s != "" {
			b.WriteString("О себе: " + s + "\n")
		}
	}
	if langs := languageList(r); len(langs) > 0 {
		b.WriteString("Языки: " + strings.Join(langs, "; ") + "\n")
	}
	if lines := experienceLines(r.Experience); len(lines) > 0 {
		b.WriteString("Опыт работы:\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// nameList merges the plural field with its singular alias, preserving
// order and skipping empty names.
func nameList(many []nameRef, single *nameRef) []string {
	out := make([]string, 0, len(many)+1)
	for _, n := range many {
		if n.Name != "" {
			out = append(out, n.Name)
		}
	}
	if single != nil && single.Name != "" {
		out = append(out, single.Name)
	}
	return out
}

func refNames(refs []nameRef) []string {
	out := make([]string, 0, len(refs))
	for _, n := range refs {
		if n.Name != "" {
			out = append(out, n.Name)
		}
	}
	return out
}

// skillsList prefers skill_set (plain strings) and falls back to key_skills.
func skillsList(r rawResume) []string {
	out := make([]string, 0, len(r.SkillSet))
	for _, s := range r.SkillSet {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	return refNames(r.KeySkills)
}

func languageList(r rawResume) []string {
	langs := r.Language
	if len(r.Languages) > 0 {
		langs = r.Languages
	}
	out := make([]string, 0, len(langs))
	for _, l := range langs {
		if l.Name == "" {
			continue
		}
		item := l.Name
		if l.Level != nil && l.Level.Name != "" {
			item += " — " + l.Level.Name
		}
		out = append(out, item)
	}
	return out
}

// experienceLines renders each experience item as
// "- position, company, area, start — end" plus an indented description
// line; items that carry no information at all are skipped.
func experienceLines(items []expRef) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		var parts []string
		if it.Position != nil && *it.Position != "" {
			parts = append(parts, *it.Position)
		}
		if it.Company != nil && *it.Company != "" {
			parts = append(parts, *it.Company)
		}
		if it.Area != nil && it.Area.Name != "" {
			parts = append(parts, it.Area.Name)
		}
		if period := periodLine(it.Start, it.End); period != "" {
			parts = append(parts, period)
		}
		description := ""
		if it.Description != nil {
			description = clipLine(oneLine(*it.Description), maxDigestLine)
		}
		if len(parts) == 0 && description == "" {
			continue
		}
		line := "- " + strings.Join(parts, ", ")
		if description != "" {
			line += "\n  " + description
		}
		out = append(out, line)
	}
	return out
}

// periodLine formats the work period; an absent end means "present".
func periodLine(start, end *string) string {
	s := monthYear(start)
	e := monthYear(end)
	switch {
	case s == "" && e == "":
		return ""
	case s == "":
		return "до " + e
	case e == "":
		return s + " — по настоящее время"
	default:
		return s + " — " + e
	}
}

// monthYear normalizes an hh date ("2021-03-01") to "YYYY-MM"; anything
// that does not match the pattern is passed through trimmed.
func monthYear(v *string) string {
	if v == nil {
		return ""
	}
	s := strings.TrimSpace(*v)
	if len(s) >= 7 && s[4] == '-' && isDigits(s[:4]) && isDigits(s[5:7]) {
		return s[:7]
	}
	return s
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// formatMonths renders a month count as "<N> лет/года/год" plus
// "<M> мес." (months are always abbreviated to keep the digest compact).
func formatMonths(months int) string {
	if months <= 0 {
		return ""
	}
	y, m := months/12, months%12
	var parts []string
	if y > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", y, pluralRu(y, "год", "года", "лет")))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d мес.", m))
	}
	return strings.Join(parts, " ")
}

// pluralRu is the standard Russian plural rule.
func pluralRu(n int, one, few, many string) string {
	if n%10 == 1 && n%100 != 11 {
		return one
	}
	if n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14) {
		return few
	}
	return many
}

// oneLine flattens multi-line free text into a single line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", " ")
}

// clipLine trims s and clips it to max runes (append an ellipsis when
// cut). max <= 0 disables clipping.
func clipLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if max > 0 {
		if r := []rune(s); len(r) > max {
			return strings.TrimRight(string(r[:max]), " \t") + "…"
		}
	}
	return s
}

func trimSpaceBytes(b json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(b)))
}
