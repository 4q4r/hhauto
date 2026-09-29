package autoclone

// T54: the human resolution of parked needs_human verdicts. The parked
// review payload must carry the full было/стало (locator, current value,
// AI proposal), and NeedApply must push a human-approved value through
// the same apply surfaces the improve pipeline uses (browser editor when
// wired, builder-update API otherwise), with the same validation gate.

import (
	"context"
	"encoding/json"
	"testing"
)

// The parked needs_human payload records the resolution inputs: resume
// hash, field, locator, item, было (current) and стало (the AI proposal
// extracted from the verdict).
func TestNeedsHumanParkCapturesProposalAndLocator(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	rev := &fakeReviewer{}
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{
			"value_string": "DevOps 7 лет", "needs_human": true, "needs_human_reason": "number_not_in_sources",
		}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, rev)

	if _, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if len(rev.payloads) != 1 {
		t.Fatalf("parked payloads = %d", len(rev.payloads))
	}
	var p struct {
		Clone   string          `json:"clone"`
		Field   string          `json:"field"`
		Locator string          `json:"locator"`
		Item    int             `json:"item"`
		Before  json.RawMessage `json:"before"`
		After   json.RawMessage `json:"after"`
	}
	if err := json.Unmarshal([]byte(rev.payloads[0]), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.Clone != "cl0ne1" || p.Field != "title" || p.Locator != "resume.title" || p.Item != -1 {
		t.Fatalf("payload identity = %+v", p)
	}
	if string(p.Before) != `"DevOps-инженер"` {
		t.Fatalf("before = %s", p.Before)
	}
	if string(p.After) != `"DevOps 7 лет"` {
		t.Fatalf("after = %s", p.After)
	}
}

// NeedApply pushes a human-approved value through the builder-update API
// surface (the editor is not wired): validated against the live content
// first, then posted.
func TestNeedApplyAPIPath(t *testing.T) {
	ed := newFakeEditor(t)
	na := &NeedApply{API: ed}
	spec, ok := Spec("title")
	if !ok {
		t.Fatal("no title spec")
	}
	if err := na.ApplyValue(context.Background(), "cl0ne1", spec, -1, "resume.title", "DevOps-инженер 2026"); err != nil {
		t.Fatalf("ApplyValue: %v", err)
	}
	if s := condStringsT(t, ed.state.Resume["title"]); len(s) != 1 || s[0] != "DevOps-инженер 2026" {
		t.Fatalf("applied title = %v", s)
	}
}

// A value failing the live validation is rejected before any post: the
// resume keeps its current value (the review item stays open for the
// human to correct).
func TestNeedApplyValidationFailureAppliesNothing(t *testing.T) {
	ed := newFakeEditor(t)
	na := &NeedApply{API: ed}
	spec, _ := Spec("title")
	if err := na.ApplyValue(context.Background(), "cl0ne1", spec, -1, "resume.title", "Д"); err == nil {
		t.Fatal("expected validation error")
	}
	if ed.updateCount() != 0 {
		t.Fatal("a rejected value must not post")
	}
}

// With the browser editor wired, the value rides the FieldChange plan —
// the same apply surface as the improve pipeline.
func TestNeedApplyEditorPath(t *testing.T) {
	plan := &fakePlanEditor{}
	na := &NeedApply{API: newFakeEditor(t), Editor: plan}
	spec, _ := Spec("title")
	if err := na.ApplyValue(context.Background(), "cl0ne1", spec, -1, "resume.title", "DevOps-инженер 2026"); err != nil {
		t.Fatalf("ApplyValue: %v", err)
	}
	if plan.appliedHash != "cl0ne1" || len(plan.appliedPlan) != 1 {
		t.Fatalf("applied hash/plan = %q/%+v", plan.appliedHash, plan.appliedPlan)
	}
	fc := plan.appliedPlan[0]
	if fc.Field != "title" || fc.Locator != "resume.title" || fc.Item != -1 {
		t.Fatalf("field change identity = %+v", fc)
	}
	if string(fc.Value) != `"DevOps-инженер 2026"` {
		t.Fatalf("natural value = %s", fc.Value)
	}
}

// A per-change editor failure is an apply-layer error, not a silent skip.
func TestNeedApplyEditorFailureIsError(t *testing.T) {
	plan := &fakePlanEditor{fail: map[string]string{"resume.title": "selector not found"}}
	na := &NeedApply{API: newFakeEditor(t), Editor: plan}
	spec, _ := Spec("title")
	if err := na.ApplyValue(context.Background(), "cl0ne1", spec, -1, "resume.title", "DevOps-инженер 2026"); err == nil {
		t.Fatal("expected apply error")
	}
}
