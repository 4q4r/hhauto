package hhapi

// Badge label extraction. The parser lived in internal/console (T69) and
// moved here so vacancyFromItem can fill the typed Vacancy.Badges from the
// raw the daemon already holds; the console surface renders the typed field.
// Live fact-check T70 (2026-09-30): search items carry the badges under the
// company node (company.badges.badge[]), cards under employer.

import "encoding/json"

// badge is one entry of the raw badges payload (both the flat array and
// the SSR nested {badge: [...]} shape).
type badge struct {
	Type string `json:"type"`
}

// badgeLabels maps the badge types known from the live fixtures onto
// Russian labels (T69). Unknown types render as-is.
var badgeLabels = map[string]string{
	"employer-it-accreditation": "аккредитованный ИТ", // company.badges (search item) / popup shortVacancy.employer.badges
	"accredited_it_employer":    "аккредитованный ИТ", // JSON-API spelling of the same fact
	"employer-hh-rating":        "рейтинг hh",         // company.badges (search item) / employer badges (chats-list)
}

// Badges extracts the badge labels from a vacancy raw payload. Checked
// shapes: a top-level badges array, its SSR nesting {badge: [...]} and the
// same two shapes under employer (cards) and company (search items,
// live-verified 2026-09-30).
func Badges(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		Badges   json.RawMessage `json:"badges"`
		Employer struct {
			Badges json.RawMessage `json:"badges"`
		} `json:"employer"`
		Company struct {
			Badges json.RawMessage `json:"badges"`
		} `json:"company"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	for _, field := range []json.RawMessage{doc.Badges, doc.Employer.Badges, doc.Company.Badges} {
		if labels := badgeList(field); len(labels) > 0 {
			return labels
		}
	}
	return nil
}

// badgeList parses one badges field: a flat array first, then the SSR
// {badge: [...]} nesting.
func badgeList(field json.RawMessage) []string {
	if len(field) == 0 {
		return nil
	}
	var flat []badge
	if err := json.Unmarshal(field, &flat); err == nil {
		return labelBadges(flat)
	}
	var nested struct {
		Badge []badge `json:"badge"`
	}
	if err := json.Unmarshal(field, &nested); err == nil {
		return labelBadges(nested.Badge)
	}
	return nil
}

// labelBadges maps badge types onto labels; unknown types pass through
// as-is.
func labelBadges(list []badge) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, b := range list {
		if b.Type == "" {
			continue
		}
		if label, ok := badgeLabels[b.Type]; ok {
			out = append(out, label)
			continue
		}
		out = append(out, b.Type)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
