package profile

// Helpers for the human review flow: turning "questions without facts"
// into user-facing prompts, detecting the missing-data / needs-human
// markers inside AI verdict JSON, and suggesting the starter profile.md
// question set. Missing facts are never invented — they surface as
// questions the user answers manually or adds to profile.md.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// onboardingQuestions is the fixed starter set for the first profile.md
// fill: target roles, salary expectations, forbidden topics, languages,
// relocation readiness and preferred work format.
var onboardingQuestions = []string{
	"Какие роли и позиции считать целевыми?",
	"Какая желаемая вилка по зарплате (минимум и комфортный уровень)?",
	"Какие темы запрещено упоминать в откликах и переписке?",
	"Какими языками владеешь и на каком уровне?",
	"Готов ли к релокации или командировкам? В какие города?",
	"Какой формат работы предпочитаешь: офис, гибрид или удалёнка?",
}

// SuggestOnboardingQuestions returns the fixed starter question set for
// filling profile.md (rendered by the CLI). The caller receives a fresh
// copy; mutating it does not affect the package.
func SuggestOnboardingQuestions() []string {
	out := make([]string, len(onboardingQuestions))
	copy(out, onboardingQuestions)
	return out
}

// MissingFactsPrompt converts questions that have no stored fact into
// user-facing prompts. Blank entries and duplicates are dropped; the order
// of first occurrence is preserved.
func MissingFactsPrompt(questions []string) []string {
	out := make([]string, 0, len(questions))
	seen := make(map[string]bool, len(questions))
	for _, q := range questions {
		q = strings.TrimSpace(q)
		if q == "" || seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, fmt.Sprintf("Нет данных для ответа: «%s». Ответь вручную или допиши ответ в profile.md (hhauto profile edit).", q))
	}
	return out
}

// aiVerdict is the subset of the AI verdict JSON DetectMissing understands.
type aiVerdict struct {
	MissingData      []string `json:"missing_data"`
	NeedsHumanReason *string  `json:"needs_human_reason"`
}

// DetectMissing extracts the missing_data[] items and the needs_human_reason
// from an AI verdict. It is tolerant of absent fields: nil/empty input and
// verdicts without these fields yield (nil, nil). Malformed JSON or wrong
// field types surface as errors — a schema violation must not be swallowed.
func DetectMissing(output json.RawMessage) ([]string, error) {
	if len(output) == 0 {
		return nil, nil
	}
	var v aiVerdict
	if err := json.Unmarshal(output, &v); err != nil {
		return nil, fmt.Errorf("profile: parse AI verdict: %w", err)
	}
	out := make([]string, 0, len(v.MissingData)+1)
	seen := make(map[string]bool, len(v.MissingData)+1)
	add := func(s string) {
		if s = strings.TrimSpace(s); s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range v.MissingData {
		add(m)
	}
	if v.NeedsHumanReason != nil {
		add(*v.NeedsHumanReason)
	}
	return out, nil
}
