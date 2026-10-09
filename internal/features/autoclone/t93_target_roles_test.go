package autoclone

// T93 (а): the target-roles parameter rides EVERY improve task — the
// profile.md «Целевые роли» section is the deterministic source it never
// had (RC-1).

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"hhauto/internal/profile"
)

func TestImprovePassesTargetRolesToEveryTask(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "b4se123", "cloneR")
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (Docker)"}),
		"about": improveVerdictFor(t, map[string]any{"unchanged": true}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, nil)
	im.opt.ProfileFor = func(context.Context, string) (profile.Profile, error) {
		return profile.Profile{ProfileText: `
## Целевые роли
- Разработка: fullstack / backend / Go
- DevOps / DevSecOps
- QA (автотесты)
`}, nil
	}
	if _, err := im.ImproveClone(t.Context(), "acc1", "b4se123", "cloneR"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	want := []string{"fullstack", "backend", "Go", "DevOps", "DevSecOps", "QA (автотесты)"}
	for _, field := range []string{"title", "about"} {
		in, ok := run.inputs[field]
		if !ok {
			t.Fatalf("no %s call captured", field)
		}
		if !reflect.DeepEqual(in.TargetRoles, want) {
			t.Fatalf("%s: TargetRoles = %#v, want %#v", field, in.TargetRoles, want)
		}
	}
}

// No profile → no target_roles (the parameter stays absent, never a
// fabricated list), and the run does not fail.
func TestImproveWithoutProfileOmitsTargetRoles(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (Docker)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.ProfileFor = func(context.Context, string) (profile.Profile, error) {
		return profile.Profile{}, errors.New("no profile")
	}
	if _, err := im.ImproveAnyResume(t.Context(), "acc1", "b4se123"); err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if in := run.inputs["title"]; len(in.TargetRoles) != 0 {
		t.Fatalf("TargetRoles = %#v, want empty", in.TargetRoles)
	}
	if len(run.inputs["title"].CurrentValue) == 0 {
		t.Fatal("current value lost")
	}
}
