// Package autoclone implements the T29 clone-improvement feature: the
// mutable-field model of an hh resume, per-field AI improvement (builtin
// prompts + TOML overrides, facts are never invented), the apply through
// the cracked builder update endpoint with a verify gate and a diff
// artifact (было/стало per field), and the clone_original_action policy
// (keep | hide | delete) over the source resume.
//
// Invariants: base resumes are never mutated (content updates ride clone
// hashes only, proven against the local lineage), every AI value is
// grounded on resume/profile facts (numbers and dates come from the source
// or the owner's rules — the deterministic post-checks force needs_human
// otherwise), and the audit diff is stored on the clone's lineage row.
package autoclone

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"hhauto/internal/hhapi"
)

// FieldKind classifies the value shape and validation rules of a field.
type FieldKind string

const (
	// KindText is a scalar string value stored in condition format
	// ([{"string": v}]) or plain (experience item internals).
	KindText FieldKind = "text"
	// KindTextList is a list of strings in condition format.
	KindTextList FieldKind = "text_list"
	// KindSalary is the salary expectation [{amount, currency}].
	KindSalary FieldKind = "salary"
	// KindDate is date data (single birthday or an experience range).
	KindDate FieldKind = "date"
	// KindStructured is a dictionary-backed value passed through verbatim
	// (role ids, citizenship, education items, ...): never AI-improvable.
	KindStructured FieldKind = "structured"
)

// AI verdict slots (internal/ai resume_improve schema).
const (
	slotString = "value_string"
	slotList   = "value_strings"
	slotAmount = "value_amount"
)

// FieldSpec describes one mutable resume field: where it lives in the
// update payload, how values are validated, which wizard screens persist
// it, and whether it is AI-improvable (a builtin prompt means yes; date
// fields carry no builtin prompt — improving one requires an explicit
// owner prompt override).
type FieldSpec struct {
	ID      string
	Label   string    // human name (RU) for prompts and logs
	Kind    FieldKind // value kind
	Side    string    // "resume" | "profile" (the hM split)
	Key     string    // state key inside the side
	Screens []string  // candidate wizard screens (intersected with the live screens)
	Prompt  string    // builtin AI prompt; "" = not AI-improvable by default
	Slot    string    // AI verdict slot (value_string | value_strings | value_amount)
	// ExpPart addresses a component of an experience item (the field
	// expands to one target per item index).
	ExpPart string // "" | companyName | position | description | dates
	// FactRisky marks fields where an AI change can alter facts (salary).
	FactRisky bool
}

// hh title limits (bundle _conditions, live 2026-09-25).
const (
	titleMinLength = 2
	titleMaxLength = 100
	// Fallbacks when the live conditions are missing.
	listFallbackMaxCount = 30
)

func titleSpec() FieldSpec {
	return FieldSpec{
		ID: "title", Label: "Заголовок резюме", Kind: KindText, Side: "resume", Key: "title",
		Screens: []string{"title", "title_and_professional_role"},
		Prompt: `Ты улучшаешь заголовок резюме (поле hh.ru «Должность»). Задача: обогатить заголовок
ключевыми словами целевых ролей, сохранив фактическую должность кандидата. Заголовок должен
читаться как название должности (2–100 символов), без компаний, зарплат, обращений и ссылок.
Можно уточнить специализацию только если она видна из текущего заголовка или фактов.
Если безопасного улучшения нет — unchanged.`,
		Slot: slotString,
	}
}

func aboutSpec() FieldSpec {
	return FieldSpec{
		ID: "about", Label: "О себе", Kind: KindText, Side: "resume", Key: "skills",
		Screens: []string{"about", "about_me"},
		Prompt: `Ты улучшаешь поле hh.ru «О себе». Задача: переработать текст в связное, уверенное
описание стилистики кандидата с ключевыми словами целевых ролей. Используй ТОЛЬКО факты из
текущего значения и проверенных фактов: никакие новые числа (стаж, размеры команд, проценты),
даты, места работы и технологии не допускаются. Структура: 1) кто я и чем силён,
2) ключевые технологии/навыки списком через запятую, 3) чем полезен работодателю.
Без ссылок, контактов, зарплат и обращений. Если фактов мало для безопасного улучшения — unchanged.`,
		Slot: slotString,
	}
}

func keySkillsSpec() FieldSpec {
	return FieldSpec{
		ID: "key_skills", Label: "Ключевые навыки", Kind: KindTextList, Side: "resume", Key: "keySkills",
		Screens: []string{"key_skills"},
		Prompt: `Ты улучшаешь список ключевых навыков резюме hh.ru. Задача: дополнить и уточнить список
навыками, которые ОЧЕВИДНО следуют из фактов кандидата (из текущих навыков, описания опыта и
целевых ролей — например, инструмент, прямо названный в описании опыта). Не добавляй навыки,
которых нет в фактах. Сохраняй существующие навыки. Формат: короткие названия без пояснений.
Если расширять нечем — unchanged.`,
		Slot: slotList,
	}
}

func salarySpec() FieldSpec {
	return FieldSpec{
		ID: "salary", Label: "Зарплатные ожидания", Kind: KindSalary, Side: "resume", Key: "salary",
		Screens: []string{"position_and_salary", "salary", "common"},
		Prompt: `Ты подгоняешь зарплатные ожидания в резюме hh.ru под целевую вилку владельца из
проверенных фактов (правила переданы в фактах точными числами). Верни ОДНО число — сумму
внутри вилки, максимально близкую к её верхней границе (рынок читает «ожидания» как минимум
вилки). Если вилка в фактах не задана точными числами — unchanged. Никогда не придумывай
число вне вилки.`,
		Slot:      slotAmount,
		FactRisky: true,
	}
}

func expDescSpec() FieldSpec {
	return FieldSpec{
		ID: "experience_description", Label: "Описание опыта", Kind: KindText, Side: "profile", Key: "experience",
		Screens: []string{"experience", "about_me_and_experience"}, ExpPart: "description",
		Prompt: `Ты усиливаешь описание одного места работы в резюме hh.ru. Формула каждой строки:
«действие → результат → метрика», где метрика берётся ТОЛЬКО из фактов (числа из текущего
текста или проверенных фактов; новых чисел нет). Заменяй пассивные формулировки на активные,
начинай с глагола, группируй по проектам. Сохрани все технологии и факты исходного текста.
Без ссылок и обращений. Если усиливать нечем безопасно — unchanged.`,
		Slot: slotString,
	}
}

func expDatesSpec() FieldSpec {
	return FieldSpec{
		ID: "experience_dates", Label: "Даты опыта", Kind: KindDate, Side: "profile", Key: "experience",
		Screens: []string{"experience", "about_me_and_experience"}, ExpPart: "dates",
		// No builtin prompt by owner decision: dates are not an AI field.
		// Improving them requires an explicit owner prompt override.
	}
}

// fieldSpecs is the full mutable-field model of an hh resume (T29: «всё,
// что можно поменять»). The photo field is excluded — it is a binary
// upload, not a value mutation.
var fieldSpecs = []FieldSpec{
	titleSpec(),
	aboutSpec(),
	keySkillsSpec(),
	salarySpec(),
	expDescSpec(),
	expDatesSpec(),
	// Experience item components (factual, not AI-improvable by default).
	{ID: "experience_company", Label: "Компания", Kind: KindText, Side: "profile", Key: "experience", ExpPart: "companyName", Screens: []string{"experience", "about_me_and_experience"}},
	{ID: "experience_position", Label: "Должность", Kind: KindText, Side: "profile", Key: "experience", ExpPart: "position", Screens: []string{"experience", "about_me_and_experience"}},
	// Professional roles and work conditions: dictionary-backed values.
	{ID: "professional_roles", Label: "Профессиональные роли", Kind: KindStructured, Side: "resume", Key: "professionalRole", Screens: []string{"professional_role", "title_and_professional_role"}},
	{ID: "work_schedule", Label: "График работы", Kind: KindStructured, Side: "resume", Key: "workSchedule", Screens: []string{"common"}},
	{ID: "employment", Label: "Вид занятости", Kind: KindStructured, Side: "resume", Key: "employment", Screens: []string{"common"}},
	{ID: "work_formats", Label: "Форматы работы", Kind: KindStructured, Side: "resume", Key: "workFormats", Screens: []string{"common"}},
	// Personal profile fields.
	{ID: "area", Label: "Город проживания", Kind: KindStructured, Side: "profile", Key: "area", Screens: []string{"personal", "area"}},
	{ID: "metro", Label: "Метро", Kind: KindStructured, Side: "profile", Key: "metro", Screens: []string{"personal", "area"}},
	{ID: "citizenship", Label: "Гражданство", Kind: KindStructured, Side: "profile", Key: "citizenship", Screens: []string{"personal", "citizenship"}},
	{ID: "gender", Label: "Пол", Kind: KindStructured, Side: "profile", Key: "gender", Screens: []string{"personal"}},
	{ID: "birthday", Label: "Дата рождения", Kind: KindDate, Side: "profile", Key: "birthday", Screens: []string{"personal"}},
	{ID: "relocation", Label: "Переезд", Kind: KindStructured, Side: "profile", Key: "relocation", Screens: []string{"personal", "relocation"}},
	{ID: "work_ticket", Label: "Разрешение на работу", Kind: KindStructured, Side: "profile", Key: "workTicket", Screens: []string{"personal"}},
	{ID: "driver_license", Label: "Водительские права", Kind: KindStructured, Side: "profile", Key: "driverLicenseTypes", Screens: []string{"personal", "driving"}},
	{ID: "language", Label: "Языки", Kind: KindStructured, Side: "profile", Key: "language", Screens: []string{"personal", "languages"}},
	// Education blocks (structured item lists).
	{ID: "education_primary", Label: "Высшее образование", Kind: KindStructured, Side: "profile", Key: "primaryEducation", Screens: []string{"education", "primary_education"}},
	{ID: "education_elementary", Label: "Среднее образование", Kind: KindStructured, Side: "profile", Key: "elementaryEducation", Screens: []string{"education", "elementary_education"}},
	{ID: "education_additional", Label: "Повышение квалификации", Kind: KindStructured, Side: "profile", Key: "additionalEducation", Screens: []string{"education", "additional_education"}},
	{ID: "education_attestation", Label: "Аттестация", Kind: KindStructured, Side: "profile", Key: "attestationEducation", Screens: []string{"education", "attestation_education"}},
}

// specIndex resolves specs by id.
var specIndex = func() map[string]FieldSpec {
	m := make(map[string]FieldSpec, len(fieldSpecs))
	for _, s := range fieldSpecs {
		m[s.ID] = s
	}
	return m
}()

// Spec returns the model spec of one field id.
func Spec(id string) (FieldSpec, bool) {
	s, ok := specIndex[id]
	return s, ok
}

// AllFieldIDs returns every mutable field id (model completeness).
func AllFieldIDs() []string {
	ids := make([]string, 0, len(fieldSpecs))
	for _, s := range fieldSpecs {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

// PromptableField reports whether the field accepts an AI prompt at all:
// builtin-AI fields plus date fields (owner override only). Dictionary
// fields never accept prompts — the AI cannot ground dictionary ids.
func PromptableField(id string) bool {
	s, ok := specIndex[id]
	if !ok {
		return false
	}
	return s.Prompt != "" || s.Kind == KindDate
}

// screensFor resolves the wizard screens that persist one field: the
// candidates intersected with the resume's live screens; no intersection →
// all live screens (the SPA posts the whole accumulated form from every
// screen, so any real screen persists the payload).
func screensFor(id string, content *hhapi.ResumeContent) []string {
	spec, ok := specIndex[id]
	if !ok || len(content.Screens) == 0 {
		return content.Screens
	}
	live := map[string]bool{}
	for _, s := range content.Screens {
		live[s] = true
	}
	var out []string
	for _, s := range spec.Screens {
		if live[s] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return content.Screens
	}
	return out
}

// --- value access -----------------------------------------------------------

// readValue extracts the current value of one field target from the split
// content, unwrapped to its natural JSON shape (string / []string /
// {amount,currency} / item map / raw structured).
func readValue(spec FieldSpec, content *hhapi.ResumeContent, itemIndex int) (json.RawMessage, error) {
	side := sideMap(content, spec.Side)
	raw, ok := side[spec.Key]
	if !ok {
		return nil, fmt.Errorf("field %s: key %q missing from the live state", spec.ID, spec.Key)
	}
	if spec.Key != "experience" {
		return raw, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("field %s: decode experience: %w", spec.ID, err)
	}
	if itemIndex < 0 || itemIndex >= len(items) {
		return nil, fmt.Errorf("field %s: item %d out of range (%d items)", spec.ID, itemIndex, len(items))
	}
	part := items[itemIndex][expPartKey(spec)]
	if len(part) == 0 {
		return json.RawMessage(`null`), nil
	}
	return part, nil
}

func expPartKey(spec FieldSpec) string {
	switch spec.ExpPart {
	case "dates":
		return "startDate" // dates read as the pair; readValue for dates is specialized
	case "":
		return ""
	default:
		return spec.ExpPart
	}
}

func sideMap(content *hhapi.ResumeContent, side string) map[string]json.RawMessage {
	if side == "profile" {
		return content.Profile
	}
	return content.Resume
}

// unwrapValue renders the stored raw value as the natural scalar the AI
// and validators work with: condition-format strings become plain strings,
// keySkills a plain string list, salary {amount,currency}, experience
// parts stay plain.
func unwrapValue(spec FieldSpec, raw json.RawMessage) json.RawMessage {
	switch spec.Kind {
	case KindText:
		if spec.ExpPart != "" {
			return raw // plain item component
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
			return json.RawMessage(`null`)
		}
		return parts[0]["string"]
	case KindTextList:
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &parts); err != nil {
			return json.RawMessage(`null`)
		}
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			var s string
			if err := json.Unmarshal(p["string"], &s); err == nil {
				out = append(out, s)
			}
		}
		b, _ := json.Marshal(out)
		return b
	case KindSalary:
		var arr []struct {
			Amount   *float64 `json:"amount"`
			Currency string   `json:"currency"`
		}
		if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
			return json.RawMessage(`{"amount":null,"currency":"RUR"}`)
		}
		amt := any(nil)
		if arr[0].Amount != nil {
			amt = *arr[0].Amount
		}
		cur := arr[0].Currency
		if cur == "" {
			cur = "RUR"
		}
		b, _ := json.Marshal(map[string]any{"amount": amt, "currency": cur})
		return b
	default:
		return raw
	}
}

// conditionLimits reads the live hh limits for a key:
// (minlength, maxlength, maxcount, ok). Missing conditions → ok=false.
func conditionLimits(content *hhapi.ResumeContent, key string) (minLen, maxLen, maxCount int, ok bool) {
	raw, present := content.Conditions[key]
	if !present {
		return 0, 0, 0, false
	}
	var c struct {
		Parts []struct {
			String struct {
				MinLength int `json:"minlength"`
				MaxLength int `json:"maxlength"`
				MaxCount  int `json:"maxcount"`
			} `json:"string"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || len(c.Parts) == 0 {
		return 0, 0, 0, false
	}
	p := c.Parts[0].String
	return p.MinLength, p.MaxLength, p.MaxCount, true
}

// ValidateValue validates (and normalizes) a proposed field value against
// the live hh contract: dates end≥start and no overlaps, salary int≥0 with
// the currency riding from the current value, strings within the live
// condition limits (static hh fallbacks for title). The returned raw value
// is ready to be written into the update payload.
func ValidateValue(spec FieldSpec, content *hhapi.ResumeContent, proposed any) (json.RawMessage, error) {
	switch spec.Kind {
	case KindText:
		return validateTextValue(spec, content, proposed)
	case KindTextList:
		return validateTextListValue(spec, content, proposed)
	case KindSalary:
		return validateSalaryValue(spec, content, proposed)
	case KindDate:
		return validateDateValue(spec, content, proposed)
	default: // KindStructured
		return validateStructuredValue(spec, proposed)
	}
}

// validateTextValue trims and bounds one scalar text value.
func validateTextValue(spec FieldSpec, content *hhapi.ResumeContent, proposed any) (json.RawMessage, error) {
	s, ok := proposed.(string)
	if !ok {
		return nil, fmt.Errorf("field %s: value must be a string", spec.ID)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("field %s: empty value", spec.ID)
	}
	if spec.ID == "title" {
		minLen, maxLen, _, live := conditionLimits(content, "title")
		if !live {
			minLen, maxLen = titleMinLength, titleMaxLength
		}
		if l := len([]rune(s)); l < minLen || l > maxLen {
			return nil, fmt.Errorf("field %s: length %d outside [%d,%d]", spec.ID, l, minLen, maxLen)
		}
	}
	if spec.ExpPart != "" {
		b, _ := json.Marshal(s)
		return b, nil
	}
	b, _ := json.Marshal([]map[string]string{{"string": s}})
	return b, nil
}

// validateTextListValue trims, dedupes and bounds one list value.
func validateTextListValue(spec FieldSpec, content *hhapi.ResumeContent, proposed any) (json.RawMessage, error) {
	items, ok := proposed.([]string)
	if !ok {
		return nil, fmt.Errorf("field %s: value must be a list of strings", spec.ID)
	}
	clean := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		clean = append(clean, it)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("field %s: empty list", spec.ID)
	}
	_, _, maxCount, live := conditionLimits(content, spec.Key)
	if !live {
		maxCount = listFallbackMaxCount
	}
	if maxCount > 0 && len(clean) > maxCount {
		return nil, fmt.Errorf("field %s: %d items over maxcount %d", spec.ID, len(clean), maxCount)
	}
	parts := make([]map[string]string, 0, len(clean))
	for _, it := range clean {
		parts = append(parts, map[string]string{"string": it})
	}
	b, _ := json.Marshal(parts)
	return b, nil
}

// validateSalaryValue bounds the amount and rides the currency from the
// current value.
func validateSalaryValue(spec FieldSpec, content *hhapi.ResumeContent, proposed any) (json.RawMessage, error) {
	amount, ok := toAmount(proposed)
	if !ok {
		return nil, fmt.Errorf("field %s: value must be a non-negative integer", spec.ID)
	}
	// The currency rides from the current value (hh codes it RUR even
	// for rubles; changing the currency is a fact change, not a nudge).
	currency := "RUR"
	if cur, err := readValue(spec, content, -1); err == nil {
		var arr []struct {
			Currency string `json:"currency"`
		}
		if json.Unmarshal(cur, &arr) == nil && len(arr) > 0 && arr[0].Currency != "" {
			currency = arr[0].Currency
		}
	}
	b, _ := json.Marshal([]map[string]any{{"amount": amount, "currency": currency}})
	return b, nil
}

// validateStructuredValue passes a dictionary-backed list through.
func validateStructuredValue(spec FieldSpec, proposed any) (json.RawMessage, error) {
	b, err := json.Marshal(proposed)
	if err != nil {
		return nil, fmt.Errorf("field %s: encode value: %w", spec.ID, err)
	}
	var arr []any
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, fmt.Errorf("field %s: value must be a JSON array (dictionary-backed)", spec.ID)
	}
	return b, nil
}

// toAmount coerces the proposed salary into a non-negative integer.
func toAmount(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, false
		}
		return n, true
	case float64:
		if n < 0 || n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil || f < 0 || f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// validateDateValue handles the two date shapes: an experience range
// {startDate, endDate} (per item) and a single birthday ("YYYY-MM-DD").
func validateDateValue(spec FieldSpec, content *hhapi.ResumeContent, proposed any) (json.RawMessage, error) {
	if spec.Key == "birthday" {
		s, ok := proposed.(string)
		if !ok || !validDate(s) {
			return nil, fmt.Errorf("field %s: date must be YYYY-MM-DD", spec.ID)
		}
		b, _ := json.Marshal([]map[string]string{{"date": s}})
		return b, nil
	}
	// Experience range.
	return validateExperienceRange(spec, content, -1, proposed)
}

// ValidateValueAt is ValidateValue with the experience item index the
// value applies to (the replaced item is excluded from the overlap check).
func ValidateValueAt(spec FieldSpec, content *hhapi.ResumeContent, itemIndex int, proposed any) (json.RawMessage, error) {
	if spec.Kind == KindDate && spec.Key == "experience" {
		return validateExperienceRange(spec, content, itemIndex, proposed)
	}
	return ValidateValue(spec, content, proposed)
}

// validateExperienceRange validates a {startDate, endDate} replacement for
// experience item itemIndex.
func validateExperienceRange(spec FieldSpec, content *hhapi.ResumeContent, itemIndex int, proposed any) (json.RawMessage, error) {
	m, ok := proposed.(map[string]*string)
	if !ok {
		return nil, fmt.Errorf("field %s: value must be {startDate, endDate}", spec.ID)
	}
	start, end := m["startDate"], m["endDate"]
	if start == nil || !validDate(*start) {
		return nil, fmt.Errorf("field %s: startDate must be YYYY-MM-DD", spec.ID)
	}
	if end != nil && *end != "" && !validDate(*end) {
		return nil, fmt.Errorf("field %s: endDate must be YYYY-MM-DD or null", spec.ID)
	}
	if end != nil && *end != "" && *end < *start {
		return nil, fmt.Errorf("field %s: end %s before start %s", spec.ID, *end, *start)
	}
	// Overlap against the OTHER items (the proposed range replaces its own
	// item); touching boundaries do not overlap.
	if err := checkExperienceOverlap(content, itemIndex, *start, endOrEmpty(end)); err != nil {
		return nil, fmt.Errorf("field %s: %v", spec.ID, err)
	}
	b, _ := json.Marshal(map[string]*string{"startDate": start, "endDate": end})
	return b, nil
}

func endOrEmpty(end *string) string {
	if end == nil {
		return ""
	}
	return *end
}

// validDate reports whether s is a strict YYYY-MM-DD calendar date.
func validDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return false
	}
	// time.Parse is lenient about out-of-range components when their text
	// shape matches; re-format and compare to reject "2020-13-40".
	return time.Date(
		int(s[0]-'0')*1000+int(s[1]-'0')*100+int(s[2]-'0')*10+int(s[3]-'0'),
		time.Month(int(s[5]-'0')*10+int(s[6]-'0')),
		int(s[8]-'0')*10+int(s[9]-'0'),
		0, 0, 0, 0, time.UTC,
	).Format("2006-01-02") == s
}

// checkExperienceOverlap verifies the proposed range against every other
// experience item in the content (the replaced item excluded; touching
// boundaries do not overlap). Empty end = open range (+inf sentinel).
func checkExperienceOverlap(content *hhapi.ResumeContent, exclude int, start, end string) error {
	raw, ok := content.Profile["experience"]
	if !ok {
		return nil
	}
	var items []struct {
		StartDate string  `json:"startDate"`
		EndDate   *string `json:"endDate"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode experience: %w", err)
	}
	const inf = "9999-12-31"
	if end == "" {
		end = inf
	}
	for i, it := range items {
		if i == exclude || it.StartDate == "" {
			continue
		}
		otherEnd := inf
		if it.EndDate != nil && *it.EndDate != "" {
			otherEnd = *it.EndDate
		}
		// A = [start, end], B = [it.StartDate, otherEnd]; overlap iff
		// start < otherEnd && it.StartDate < end (touching is fine).
		if start < otherEnd && it.StartDate < end {
			return fmt.Errorf("range %s..%s overlaps item %d (%s..%s)", start, end, i, it.StartDate, otherEnd)
		}
	}
	return nil
}
