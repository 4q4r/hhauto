package autoclone

// T54: the human resolution of parked needs_human verdicts. The human's
// apply decision reuses the improve pipeline's apply surfaces verbatim —
// the browser editor runner when wired, the builder-update API otherwise —
// including the live-content validation gate. The parked review item's
// value was never anti-hallucination-checked (that is why it parked), so
// the check here is deliberately the ordinary validation only: the human
// already made the call.

import (
	"context"
	"fmt"

	"hhauto/internal/hhapi"
)

// NeedApply applies one human-approved value to one resume field through
// the same surfaces the improve pipeline applies its plan with.
type NeedApply struct {
	API hhapi.ResumeContentEditor
	// Editor is the browser editor runner; nil = the builder-update API
	// path (the improve pipeline's fallback surface).
	Editor EditorRunner
	// DryRun stops after the live-content validation gate: nothing is
	// posted, nothing is resolved.
	DryRun bool
}

// ApplyValue validates value against the live content and applies it to
// the field target (spec+item+locator). An apply-layer failure (read,
// validation, editor error, per-change failure) returns as an error — the
// caller keeps the review item open.
func (na *NeedApply) ApplyValue(ctx context.Context, hash string, spec FieldSpec, item int, locator string, value any) error {
	live, err := na.API.ResumeContent(ctx, hash)
	if err != nil {
		return fmt.Errorf("autoclone: live content: %w", err)
	}
	validated, err := ValidateValueAt(spec, live, item, value)
	if err != nil {
		return fmt.Errorf("autoclone: %w", err)
	}
	if na.DryRun {
		return nil
	}
	if na.Editor != nil {
		res, err := na.Editor.ApplyFieldPlan(ctx, hash, []FieldChange{{
			Field: spec.ID, Locator: locator, Screen: editorScreensFor(spec.ID, live),
			Kind: spec.Kind, Label: spec.Label, Item: item,
			Value: editorNaturalValue(spec, validated),
		}})
		if err != nil {
			return err
		}
		if len(res.Failed) > 0 {
			return fmt.Errorf("autoclone: apply failed: %s", res.Failed[0].Err)
		}
		return nil
	}
	mutated := live.Clone()
	if err := writeValue(spec, mutated, item, validated); err != nil {
		return fmt.Errorf("autoclone: %w", err)
	}
	return na.API.UpdateResumeContent(ctx, hash, mutated, affectedScreens([]string{spec.ID}, mutated))
}
