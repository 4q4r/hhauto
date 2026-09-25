// Per-field prompt registry (T29): builtin defaults live in the field
// specs; [features.autoclone.improve.prompts.<field>] overrides a field's
// prompt (inline | file, never both). Dates carry no builtin prompt —
// improving them requires an explicit owner prompt (owner decision: dates
// are not an AI field by default).

package autoclone

import (
	"fmt"
	"os"
	"strings"

	"hhauto/internal/config"
)

// PromptRegistry resolves the effective system prompt per field id. It is
// immutable after construction and safe for concurrent use.
type PromptRegistry struct {
	prompts map[string]string
}

// NewPromptRegistry validates the improve config's field list against the
// field model and resolves every prompt (override → builtin). Wiring-time
// fail-fast: a bad improve section must stop the process, not degrade into
// skipped fields at clone time.
func NewPromptRegistry(cfg config.AutoCloneImprove) (*PromptRegistry, error) {
	if err := validateImproveFields(&cfg); err != nil {
		return nil, err
	}
	r := &PromptRegistry{prompts: make(map[string]string, len(cfg.Fields))}
	for _, id := range cfg.Fields {
		spec, ok := Spec(id)
		if !ok {
			return nil, fmt.Errorf("autoclone: improve.fields %q: unknown field", id)
		}
		body := spec.Prompt
		if ov, has := cfg.Prompts[id]; has {
			switch {
			case ov.PromptInline != "":
				body = ov.PromptInline
			case ov.PromptFile != "":
				raw, err := os.ReadFile(ov.PromptFile)
				if err != nil {
					return nil, fmt.Errorf("autoclone: improve.prompts[%q]: %w", id, err)
				}
				body = string(raw)
			}
		}
		if strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("autoclone: improve.prompts[%q]: empty prompt", id)
		}
		r.prompts[id] = body
	}
	return r, nil
}

// Prompt returns the effective system prompt of one field.
func (r *PromptRegistry) Prompt(field string) string {
	return r.prompts[field]
}

// Fields lists the configured field ids.
func (r *PromptRegistry) Fields() []string {
	out := make([]string, 0, len(r.prompts))
	for id := range r.prompts {
		out = append(out, id)
	}
	return out
}

// validateImproveFields checks the improve field list against the field
// model: every id known, no structured (dictionary) fields, and date
// fields are rejected unless an explicit owner prompt override enables
// them.
func validateImproveFields(cfg *config.AutoCloneImprove) error {
	seen := map[string]bool{}
	for _, id := range cfg.Fields {
		if seen[id] {
			return fmt.Errorf("autoclone: improve.fields: duplicate field %q", id)
		}
		seen[id] = true
		spec, ok := Spec(id)
		if !ok {
			return fmt.Errorf("autoclone: improve.fields %q: unknown field", id)
		}
		if spec.Kind == KindStructured {
			return fmt.Errorf("autoclone: improve.fields %q: dictionary-backed fields are not AI-improvable", id)
		}
	}
	for id := range cfg.Prompts {
		if !seen[id] {
			return fmt.Errorf("autoclone: improve.prompts[%q]: field is not in improve.fields", id)
		}
		if _, ok := Spec(id); !ok {
			return fmt.Errorf("autoclone: improve.prompts[%q]: unknown field", id)
		}
		if p := cfg.Prompts[id]; p.PromptInline != "" && p.PromptFile != "" {
			return fmt.Errorf("autoclone: improve.prompts[%q]: inline and file are mutually exclusive", id)
		}
	}
	for _, id := range cfg.Fields {
		spec, _ := Spec(id)
		if spec.Kind == KindDate {
			ov, has := cfg.Prompts[id]
			if !has || (ov.PromptInline == "" && ov.PromptFile == "") {
				return fmt.Errorf("autoclone: improve.fields %q: date fields are not AI fields by default; add an explicit prompts.%s override (owner decision)", id, id)
			}
		}
	}
	return nil
}
