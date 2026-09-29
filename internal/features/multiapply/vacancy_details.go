package multiapply

// T60: the full-vacancy context for cover letters. The complete hh vacancy
// card (api.VacancyCard) is parsed into a compact dump — named fields plus
// an "other" bucket carrying every secondary field the endpoint returned —
// that feeds BOTH the AI prompt (letterInput.vacancy_details) and the
// template placeholders (same data, letterTemplateData).
//
// Token budget (documented in docs/CONFIGURATION.md): the description rides
// as HTML-stripped text capped at maxDescriptionRunes (~3k tokens); the
// "other" bucket caps each field at maxOtherKeyBytes and the whole bucket at
// maxOtherTotalBytes — oversized secondary fields are DROPPED (they are the
// genuinely optional heavy ones, e.g. long embedded payloads). The
// decorative branded_description / branded_template blocks and the named
// fields' duplicates never enter the bucket at all.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"hhauto/internal/hhapi"
)

// Token-budget caps.
const (
	maxDescriptionRunes = 12000 // description text cap (~3k tokens)
	maxOtherKeyBytes    = 800   // one secondary field cap
	maxOtherTotalBytes  = 4000  // whole "other" bucket cap
	maxContactsEntries  = 5     // contact list cap
)

// truncatedMark closes a capped description.
const truncatedMark = "\n…[обрезано]"

var (
	// scriptBlocks removes script/style elements with their content.
	scriptBlocksRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	// blockTags breaks lines at paragraph-level tags before stripping.
	blockTagsRe = regexp.MustCompile(`(?i)</?(p|div|li|tr|h[1-6])[^>]*>|<br\s*/?>`)
	// anyTag strips the remaining markup.
	anyTagRe = regexp.MustCompile(`<[^>]*>`)
	// inlineSpaces collapses intra-line whitespace (newlines preserved).
	inlineSpacesRe = regexp.MustCompile(`[^\S\n]+`)
	// blankLines collapses 3+ consecutive newlines.
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
)

// htmlToText renders an hh HTML description as plain text: script/style
// elements disappear with their content, block tags break lines, the rest of
// the markup is stripped, entities are decoded, whitespace is collapsed. The
// result is capped at maxDescriptionRunes.
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

// truncateRunes caps s at limit runes, appending the truncation mark.
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + truncatedMark
}

// vacancyDetails is the cover-letter dump of one full vacancy card.
type vacancyDetails struct {
	Title        string                     `json:"title,omitempty"`
	Salary       string                     `json:"salary,omitempty"`
	Experience   string                     `json:"experience,omitempty"`
	Employment   string                     `json:"employment,omitempty"`
	Schedule     string                     `json:"schedule,omitempty"`
	WorkFormats  string                     `json:"work_formats,omitempty"`
	KeySkills    string                     `json:"key_skills,omitempty"`
	Area         string                     `json:"area,omitempty"`
	Address      string                     `json:"address,omitempty"`
	EmployerName string                     `json:"employer_name,omitempty"`
	EmployerType string                     `json:"employer_type,omitempty"`
	Contacts     string                     `json:"contacts,omitempty"`
	Description  string                     `json:"description,omitempty"`
	Other        map[string]json.RawMessage `json:"other,omitempty"`
}

// consumedKeys are the raw-object keys extracted into the named fields (both
// hh spellings) — they never duplicate into the "other" bucket. The heavy
// decorative blocks are dropped outright.
var consumedKeys = map[string]bool{
	"id": true, "vacancyId": true, "name": true,
	"description": true, "descriptionText": true,
	"salary": true, "compensation": true,
	"experience": true, "employment": true, "schedule": true,
	"work_formats": true, "workFormats": true, "work_schedule": true,
	"key_skills": true, "keySkills": true,
	"area": true, "address": true, "metro": true,
	"employer": true, "contacts": true,
	"alternate_url": true, "alternateURL": true,
	"published_at": true, "publishedAt": true,
	"accept_temporary": true, "acceptTemporary": true,
	"has_tests": true, "hasTests": true, "has_test": true,
	"branded_description": true, "branded_template": true,
}

// buildVacancyDetails parses the full vacancy card. The parser is tolerant
// (hh shapes drift between surfaces): every lookup falls back across
// spellings, and whatever is not extracted rides in Other.
func buildVacancyDetails(v hhapi.Vacancy) *vacancyDetails {
	d := &vacancyDetails{
		Title:        firstNonEmptyStr(v.Name, rawStr(v.Raw, "name")),
		Area:         firstNonEmptyStr(v.Area, namedOrRaw(v.Raw, "area")),
		EmployerName: firstNonEmptyStr(v.EmployerName, namedOrRaw(v.Raw, "employer")),
		Salary:       salaryText(v),
		Experience:   namedOrRaw(v.Raw, "experience"),
		Employment:   namedOrRaw(v.Raw, "employment"),
		Schedule:     namedOrRaw(v.Raw, "schedule"),
		WorkFormats:  joinNamed(rawArr(v.Raw, "work_formats", "workFormats", "work_schedule")),
		KeySkills:    joinNamed(rawArr(v.Raw, "key_skills", "keySkills")),
		Contacts:     contactsText(rawArr(v.Raw, "contacts")),
		Description:  htmlToText(firstNonEmptyStr(rawStr(v.Raw, "description"), rawStr(v.Raw, "descriptionText"))),
	}
	if emp := rawObj(v.Raw, "employer"); emp != nil {
		d.EmployerType = namedOrRaw(emp, "type")
	}
	d.Address = addressText(v.Raw)
	d.Other = otherFields(v.Raw)
	return d
}

// --- tiny tolerant JSON accessors (hhapi's obj/str are unexported) ------

func rawFields(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func rawObj(raw json.RawMessage, key string) json.RawMessage {
	m := rawFields(raw)
	if m == nil {
		return nil
	}
	return m[key]
}

// rawStr returns the string value of the first present key.
func rawStr(raw json.RawMessage, keys ...string) string {
	m := rawFields(raw)
	if m == nil {
		return ""
	}
	for _, k := range keys {
		var s string
		if v, ok := m[k]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return ""
}

// rawArr returns the array value of the first present key.
func rawArr(raw json.RawMessage, keys ...string) []json.RawMessage {
	m := rawFields(raw)
	if m == nil {
		return nil
	}
	for _, k := range keys {
		var a []json.RawMessage
		if v, ok := m[k]; ok && json.Unmarshal(v, &a) == nil {
			return a
		}
	}
	return nil
}

// namedOrStr renders {name: X} objects as X, plain strings as themselves.
func namedOrStr(raw json.RawMessage) string {
	if s := rawStr(raw, "name"); s != "" {
		return s
	}
	var s string
	if raw != nil && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// namedOrRaw reads key as a {name} object or a plain string.
func namedOrRaw(raw json.RawMessage, key string) string {
	return namedOrStr(rawObj(raw, key))
}

// joinNamed renders a [{name}] (or [string]) array as a comma list.
func joinNamed(items []json.RawMessage) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if s := namedOrStr(it); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// salaryText renders the compensation: "от 150000 до 250000 RUR (на руки)".
// The card shape is "salary", the search-item shape "compensation".
func salaryText(v hhapi.Vacancy) string {
	body := rawObj(v.Raw, "salary")
	if body == nil {
		body = rawObj(v.Raw, "compensation")
	}
	if body == nil {
		if v.Salary == nil {
			return ""
		}
		return salaryRange(v.Salary.From, v.Salary.To, v.Salary.Currency, v.Salary.Gross)
	}
	m := rawFields(body)
	num := func(key string) int {
		if m == nil {
			return 0
		}
		var f float64
		if v, ok := m[key]; ok && json.Unmarshal(v, &f) == nil {
			return int(f)
		}
		return 0
	}
	var gross bool
	if g, ok := m["gross"]; ok {
		_ = json.Unmarshal(g, &gross)
	}
	return salaryRange(num("from"), num("to"), rawStr(body, "currencyCode", "currency"), gross)
}

func salaryRange(from, to int, currency string, gross bool) string {
	if from == 0 && to == 0 {
		return ""
	}
	var parts []string
	if from > 0 {
		parts = append(parts, "от "+strconv.Itoa(from))
	}
	if to > 0 {
		parts = append(parts, "до "+strconv.Itoa(to))
	}
	out := strings.Join(parts, " ")
	if currency != "" {
		out += " " + currency
	}
	if gross {
		out += " (до вычета НДФЛ)"
	} else {
		out += " (на руки)"
	}
	return out
}

// addressText composes the address block: city, street building, metro.
func addressText(raw json.RawMessage) string {
	var parts []string
	if city := rawStr(rawObj(raw, "address"), "city"); city != "" {
		parts = append(parts, city)
	}
	addr := rawObj(raw, "address")
	street := rawStr(addr, "street")
	building := rawStr(addr, "building")
	if street != "" {
		parts = append(parts, strings.TrimSpace(street+" "+building))
	} else if building != "" {
		parts = append(parts, building)
	}
	if metro := namedOrStr(rawObj(addr, "metro")); metro != "" {
		parts = append(parts, "м. "+metro)
	} else if metro := namedOrStr(rawObj(raw, "metro")); metro != "" {
		parts = append(parts, "м. "+metro)
	}
	return strings.Join(parts, ", ")
}

// contactsText renders [{value, comment}] as "value (comment)" lines.
func contactsText(items []json.RawMessage) string {
	var parts []string
	for i, it := range items {
		if i >= maxContactsEntries {
			break
		}
		value := rawStr(it, "value")
		if value == "" {
			continue
		}
		if comment := rawStr(it, "comment"); comment != "" {
			parts = append(parts, value+" ("+comment+")")
		} else {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "; ")
}

// otherFields collects every secondary field the endpoint returned: keys not
// extracted into the named fields, not heavy-decorative and not empty. Each
// value is capped at maxOtherKeyBytes (oversized → dropped), the bucket at
// maxOtherTotalBytes (later keys yield).
func otherFields(raw json.RawMessage) map[string]json.RawMessage {
	m := rawFields(raw)
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]json.RawMessage)
	total := 0
	for _, k := range keys {
		if consumedKeys[k] {
			continue
		}
		v := bytes.TrimSpace(m[k])
		if emptyField(v) {
			continue
		}
		if len(v) > maxOtherKeyBytes || total+len(v) > maxOtherTotalBytes {
			continue
		}
		out[k] = json.RawMessage(v)
		total += len(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// emptyField reports null/false/0/""/[]/{} — payload noise.
func emptyField(v []byte) bool {
	switch string(v) {
	case "null", "false", "0", `""`, "[]", "{}":
		return true
	}
	return false
}

// --- template placeholders ------------------------------------------------

// letterTemplateData backs the cover_letter template placeholders (T60) —
// the same vacancy data the AI prompt receives.
type letterTemplateData struct {
	VacancyTitle       string
	VacancyDescription string
	VacancySalary      string
	VacancyExperience  string
	VacancyEmployment  string
	VacancySchedule    string
	VacancyKeySkills   string
	VacancyArea        string
	VacancyAddress     string
	VacancyURL         string
	EmployerName       string
	Contacts           string
	PublishedAt        string
}

func letterTemplateDataOf(v hhapi.Vacancy, d *vacancyDetails) letterTemplateData {
	data := letterTemplateData{
		VacancyTitle: v.Name,
		VacancyURL:   v.AlternateURL,
		EmployerName: v.EmployerName,
		VacancyArea:  v.Area,
		PublishedAt:  v.PublishedAt.UTC().Format("2006-01-02"),
	}
	if d != nil {
		data.VacancyDescription = d.Description
		data.VacancySalary = d.Salary
		data.VacancyExperience = d.Experience
		data.VacancyEmployment = d.Employment
		data.VacancySchedule = d.Schedule
		data.VacancyKeySkills = d.KeySkills
		data.VacancyAddress = d.Address
		data.Contacts = d.Contacts
		if d.Title != "" {
			data.VacancyTitle = d.Title
		}
		if d.EmployerName != "" {
			data.EmployerName = d.EmployerName
		}
		if d.Area != "" {
			data.VacancyArea = d.Area
		}
	}
	return data
}

// renderLetterTemplate executes the cover-letter template; unknown fields
// fail loudly (the caller turns the error into a skip).
func renderLetterTemplate(tmpl string, data letterTemplateData) (string, error) {
	t, err := template.New("cover_letter").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("multiapply: letter template: %w", err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("multiapply: letter template: %w", err)
	}
	return strings.TrimSpace(b.String()), nil
}

// firstNonEmptyStr returns the first non-empty string.
func firstNonEmptyStr(vals ...string) string {
	for _, s := range vals {
		if s != "" {
			return s
		}
	}
	return ""
}
