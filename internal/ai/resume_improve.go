package ai

// T29 resume_improve task contract: the input shape, the FieldPromptRunner
// extension and the deterministic grounding post-checks. The task verdict
// improves ONE resume field; every number in the improved value must come
// from the trusted facts — the post-checks enforce it deterministically
// (needs_human, never a silent accept).

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// FieldPromptRunner is the optional Runner extension for tasks whose
// system prompt is resolved per call. *runner implements it; fakes may
// ignore it when the feature under test never improves fields.
type FieldPromptRunner interface {
	Runner
	// RunWithSystem runs the task pipeline with the caller's system
	// prompt (the registry prompt is bypassed; the ledger records a
	// custom-hash prompt version).
	RunWithSystem(ctx context.Context, accountID, entityType, entityID, runID string,
		task TaskName, system string, input any) (json.RawMessage, Decision, error)
}

// ImproveInput is the canonical input for TaskResumeImprove: ONE field,
// its current value and the trusted facts the improvement may draw on.
// TrustedFacts/CurrentValue ride the system prompt (TrustedSource
// convention — the stable, cacheable, trusted prefix); everything else
// stays in the untrusted user block.
type ImproveInput struct {
	// Field is the feature-level field id ("title", "about", ...).
	Field string `json:"field"`
	// FieldLabel is the human field name for the prompt ("Заголовок резюме").
	FieldLabel string `json:"field_label,omitempty"`
	// CurrentValue is the field's current value (JSON: string, array or
	// number as the field kind dictates).
	CurrentValue json.RawMessage `json:"-"`
	// TrustedFacts are the verified facts the value may draw numbers and
	// dates from (resume/profile fragments, owner rules such as a target
	// salary range written in exact numbers).
	TrustedFacts []string `json:"-"`
	// CandidateContext is the rendered trusted candidate context
	// (profile.md text plus stored owner Q&A rules) grounding the
	// improvement; it rides the trusted block, never the user block.
	CandidateContext string `json:"-"`
	// TargetRoles are the professional roles the resume targets (grounding
	// vocabulary for title/key-skills enrichment).
	TargetRoles []string `json:"target_roles,omitempty"`
	// ExtraInstructions are the owner's explicit extra instructions.
	ExtraInstructions string `json:"extra_instructions,omitempty"`
}

// UntrustedJSON is the untrusted user-block projection of the input (the
// runner marshals ImproveInput through this).
type UntrustedJSON struct {
	Field             string   `json:"field"`
	FieldLabel        string   `json:"field_label,omitempty"`
	TargetRoles       []string `json:"target_roles,omitempty"`
	ExtraInstructions string   `json:"extra_instructions,omitempty"`
}

// MarshalJSON implements the TrustedSource convention: the trusted fields
// (CurrentValue, TrustedFacts) never enter the untrusted user block.
func (in ImproveInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(UntrustedJSON{
		Field: in.Field, FieldLabel: in.FieldLabel,
		TargetRoles: in.TargetRoles, ExtraInstructions: in.ExtraInstructions,
	})
}

// TrustedBlocks renders the trusted prefix: the current value plus the
// verified facts.
func (in ImproveInput) TrustedBlocks() string {
	var b strings.Builder
	if len(in.CurrentValue) > 0 {
		b.WriteString("# ТЕКУЩЕЕ ЗНАЧЕНИЕ ПОЛЯ\n")
		b.WriteString(string(in.CurrentValue))
		b.WriteString("\n")
	}
	if len(in.TrustedFacts) > 0 {
		b.WriteString("\n# ПРОВЕРЕННЫЕ ФАКТЫ (единственный источник чисел и дат)\n")
		for _, f := range in.TrustedFacts {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	if in.CandidateContext != "" {
		b.WriteString("\n## CANDIDATE CONTEXT (trusted)\n")
		b.WriteString(in.CandidateContext)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// normalizeImproveTypes repairs the type drift observed on GLM-5.3 (max
// reasoning): string confidence and string missing_data. String confidence
// parses into [0,1] (unparseable fail-closes to 0 — never inflates trust);
// string missing_data wraps into a one-element array (an empty string
// becomes an empty one). Canonical number/array values pass through
// untouched, and extra top-level keys (e.g. key_skills) are tolerated by
// the schema and simply ride along.
func normalizeImproveTypes(m map[string]any) {
	if s, ok := m["confidence"].(string); ok {
		m["confidence"] = normalizedConfidence(s)
	}
	switch s := m["missing_data"].(type) {
	case string:
		if strings.TrimSpace(s) == "" {
			m["missing_data"] = []any{}
		} else {
			m["missing_data"] = []any{s}
		}
	}
}

// normalizedConfidence parses a string confidence into [0,1]; an
// unparseable value fail-closes to 0 (the conservative direction: low
// confidence routes to a human, high confidence does not).
func normalizedConfidence(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// checkImproveGrounding is the deterministic post-check for
// TaskResumeImprove: any number in the improved value that is absent from
// the trusted sources forces needs_human. Empty values and unchanged
// verdicts pass.
func checkImproveGrounding(m map[string]any, forceReason, trustedText string, trustedNums map[string]bool) string {
	if unchanged, _ := m["unchanged"].(bool); unchanged {
		return forceReason
	}
	if s, _ := m["value_string"].(string); s != "" {
		if numberLeak(s, trustedNums) || forbiddenTopic(s, trustedText, trustedNums) {
			m["needs_human"] = true
			if forceReason == "" {
				return reasonNumberNotInSrc
			}
		}
	}
	if items, ok := m["value_strings"].([]any); ok && len(items) > 0 {
		joined := make([]string, 0, len(items))
		for _, it := range items {
			if s, ok := it.(string); ok {
				joined = append(joined, s)
			}
		}
		if numberLeak(strings.Join(joined, " "), trustedNums) {
			m["needs_human"] = true
			if forceReason == "" {
				return reasonNumberNotInSrc
			}
		}
	}
	if amt, ok := m["value_amount"].(json.Number); ok {
		if !trustedNums[amt.String()] {
			m["needs_human"] = true
			if forceReason == "" {
				return reasonNumberNotInSrc
			}
		}
	}
	return forceReason
}
