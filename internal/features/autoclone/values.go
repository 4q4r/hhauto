// Value plumbing for the improve pipeline: field targets (per-item
// expansion for experience), payload reads/writes, AI verdict slot
// extraction, deterministic validation-fix mapping and the trusted-facts
// builder that grounds every AI call.

package autoclone

import (
	"encoding/json"
	"fmt"
	"strings"

	"hhauto/internal/hhapi"
)

// target is one concrete mutation point: a field spec pinned to an
// experience item index (or -1 for non-item fields).
type target struct {
	spec    FieldSpec
	item    int
	locator string          // "resume.title", "profile.experience.0.description"
	raw     json.RawMessage // stored representation (condition format)
	current json.RawMessage // unwrapped value the AI sees
}

// expandTargets resolves the mutation points of one field in the content:
// one target for plain fields, one per experience item for the experience
// components.
func expandTargets(spec FieldSpec, content *hhapi.ResumeContent) []target {
	side := sideMap(content, spec.Side)
	raw, ok := side[spec.Key]
	if !ok {
		return nil
	}
	if spec.Key != "experience" || spec.ExpPart == "" {
		current := unwrapValue(spec, raw)
		return []target{{spec: spec, item: -1, locator: spec.Side + "." + spec.Key, raw: raw, current: current}}
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]target, 0, len(items))
	for i, item := range items {
		partRaw := item[expPartKey(spec)]
		if len(partRaw) == 0 {
			partRaw = json.RawMessage(`null`)
		}
		current := partRaw
		if spec.Kind == KindText {
			var s string
			if json.Unmarshal(partRaw, &s) == nil {
				current, _ = json.Marshal(s)
			}
		}
		out = append(out, target{
			spec:    spec,
			item:    i,
			locator: fmt.Sprintf("%s.%s.%d.%s", spec.Side, spec.Key, i, spec.ExpPart),
			raw:     partRaw,
			current: current,
		})
	}
	return out
}

// writeValue stores one validated value into the mutated content at the
// target point.
func writeValue(spec FieldSpec, content *hhapi.ResumeContent, itemIndex int, value json.RawMessage) error {
	side := sideMap(content, spec.Side)
	if spec.Key != "experience" || spec.ExpPart == "" {
		side[spec.Key] = value
		return nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(side[spec.Key], &items); err != nil {
		return fmt.Errorf("decode experience: %w", err)
	}
	if itemIndex < 0 || itemIndex >= len(items) {
		return fmt.Errorf("experience item %d out of range", itemIndex)
	}
	if err := writeExperiencePart(items[itemIndex], spec, value); err != nil {
		return err
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	side[spec.Key] = b
	return nil
}

// writeExperiencePart stores one component into the experience item.
func writeExperiencePart(item map[string]json.RawMessage, spec FieldSpec, value json.RawMessage) error {
	if spec.ExpPart != "dates" {
		item[spec.ExpPart] = value
		return nil
	}
	var pair struct {
		StartDate string  `json:"startDate"`
		EndDate   *string `json:"endDate"`
	}
	if err := json.Unmarshal(value, &pair); err != nil {
		return fmt.Errorf("decode date pair: %w", err)
	}
	item["startDate"], _ = json.Marshal(pair.StartDate)
	if pair.EndDate == nil {
		item["endDate"] = json.RawMessage(`null`)
	} else {
		item["endDate"], _ = json.Marshal(*pair.EndDate)
	}
	return nil
}

// slotValue extracts the AI verdict value for the field's slot.
func slotValue(spec FieldSpec, verdict json.RawMessage) (any, error) {
	var out struct {
		ValueString  *string      `json:"value_string"`
		ValueStrings []string     `json:"value_strings"`
		ValueAmount  *json.Number `json:"value_amount"`
	}
	if err := json.Unmarshal(verdict, &out); err != nil {
		return nil, fmt.Errorf("decode verdict: %w", err)
	}
	switch spec.Slot {
	case slotString:
		if out.ValueString == nil {
			return nil, fmt.Errorf("verdict carries no value_string")
		}
		return *out.ValueString, nil
	case slotList:
		if out.ValueStrings == nil {
			return nil, fmt.Errorf("verdict carries no value_strings")
		}
		return out.ValueStrings, nil
	case slotAmount:
		if out.ValueAmount == nil {
			return nil, fmt.Errorf("verdict carries no value_amount")
		}
		return *out.ValueAmount, nil
	default:
		return nil, fmt.Errorf("field %s: unknown slot %q", spec.ID, spec.Slot)
	}
}

// fixValidationValue applies the deterministic ≤1 retry fix for an hh
// validation rejection: type/format normalization (number coercion,
// whitespace/title-length trims). ok=false means the field is skipped.
func fixValidationValue(spec *FieldSpec, live *hhapi.ResumeContent, value json.RawMessage, codes []string) (json.RawMessage, bool) {
	joined := strings.Join(codes, ",")
	if spec.Kind == KindSalary {
		return fixSalaryValue(value)
	}
	if spec.ID == "title" && strings.Contains(joined, "MAXLENGTH") {
		return fixTitleMaxLength(live, value)
	}
	// Generic text: trim once.
	if spec.Kind == KindText {
		b, _ := json.Marshal(strings.TrimSpace(string(value)))
		return b, true
	}
	return nil, false
}

// fixSalaryValue coerces "180000.00"/string amounts to int and retries once.
func fixSalaryValue(value json.RawMessage) (json.RawMessage, bool) {
	var n json.Number
	if err := json.Unmarshal(value, &n); err != nil {
		return nil, false
	}
	f, err := n.Float64()
	if err != nil || f != float64(int64(f)) {
		return nil, false
	}
	b, _ := json.Marshal(int64(f))
	return b, true
}

// fixTitleMaxLength truncates the title to the live maxlength.
func fixTitleMaxLength(live *hhapi.ResumeContent, value json.RawMessage) (json.RawMessage, bool) {
	_, maxLen, _, liveOK := conditionLimits(live, "title")
	if !liveOK {
		maxLen = titleMaxLength
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(value, &parts); err != nil || len(parts) == 0 {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(parts[0]["string"], &s); err != nil {
		return nil, false
	}
	if r := []rune(s); len(r) > maxLen {
		s = strings.TrimSpace(string(r[:maxLen]))
	}
	out, _ := json.Marshal([]map[string]string{{"string": s}})
	return out, true
}

// factsFor builds the trusted-facts block of one field target: the current
// value plus the sibling resume facts numbers/dates may be grounded on.
func factsFor(spec FieldSpec, content *hhapi.ResumeContent, itemIndex int) []string {
	var facts []string
	if spec.Key == "experience" {
		facts = append(facts, experienceFacts(content, itemIndex)...)
	}
	if spec.ID == "salary" || spec.ID == "title" {
		if raw, ok := content.Resume[spec.Key]; ok {
			facts = append(facts, "Текущее значение: "+string(unwrapValue(spec, raw)))
		}
	}
	if raw, ok := content.Resume["title"]; ok && spec.ID != "title" {
		facts = append(facts, "Заголовок резюме: "+string(unwrapValue(titleSpec(), raw)))
	}
	return facts
}

// experienceFacts renders the experience block as grounding facts (the
// improved item flagged by index).
func experienceFacts(content *hhapi.ResumeContent, itemIndex int) []string {
	raw, ok := content.Profile["experience"]
	if !ok {
		return nil
	}
	var items []struct {
		CompanyName string `json:"companyName"`
		Position    string `json:"position"`
		Description string `json:"description"`
		StartDate   string `json:"startDate"`
		EndDate     string `json:"endDate"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		marker := ""
		if i == itemIndex {
			marker = " (улучшаемое место работы)"
		}
		out = append(out, fmt.Sprintf("Место работы: %s — %s (%s—%s)%s. Описание: %s",
			it.CompanyName, it.Position, it.StartDate, it.EndDate, marker, it.Description))
	}
	return out
}

// affectedScreens resolves the update walk screens for the planned fields
// (union of the per-field resolutions, in the resume's live screen order).
func affectedScreens(fieldIDs []string, content *hhapi.ResumeContent) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range fieldIDs {
		for _, s := range screensFor(id, content) {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return content.Screens
	}
	return out
}

// displayValue unwraps a stored/validated payload value back to its
// natural scalar for the diff artifact (было/стало readability).
func displayValue(spec FieldSpec, raw json.RawMessage) json.RawMessage {
	switch spec.Kind {
	case KindText:
		if spec.ExpPart != "" || spec.Key == "experience" {
			return raw
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
			return parts[0]["string"]
		}
		return raw
	case KindTextList:
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &parts); err == nil {
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				var s string
				if json.Unmarshal(p["string"], &s) == nil {
					out = append(out, s)
				}
			}
			b, _ := json.Marshal(out)
			return b
		}
		return raw
	default:
		return raw
	}
}

// restoreRaw returns the live stored value of one pending target (the
// rollback value for a rejected retry).
func restoreRaw(live *hhapi.ResumeContent, p pending) json.RawMessage {
	for _, t := range expandTargets(p.spec, live) {
		if t.locator == p.locator {
			return t.raw
		}
	}
	return nil
}
