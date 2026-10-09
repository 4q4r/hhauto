package autoclone

// T93 (б): the deterministic role-family post-check. The registry is
// conservative; the false-positive table uses the live titles of the
// owner's three resume families and the observed drift verdicts.

import (
	"context"
	"encoding/json"
	"testing"

	"hhauto/internal/hhapi"
)

func TestTitleFamiliesAndConflict(t *testing.T) {
	tests := []struct {
		name      string
		base      string // base anchor text (title + key skills)
		candidate string // proposed title
		conflict  bool
	}{
		// The three live families keep their own titles — no false positives.
		{"devops keeps devops", "Middle DevOps-инженер Docker Kubernetes", "DevOps-инженер (CI/CD, Docker, Kubernetes)", false},
		{"devops keeps sre (same infra family)", "Middle DevOps-инженер", "SRE-инженер (Kubernetes, наблюдаемость)", false},
		{"backend keeps backend", "Backend-разработчик (Python) FastAPI", "Python Backend Developer (FastAPI, PostgreSQL, Docker)", false},
		{"fullstack keeps fullstack", "Fullstack-разработчик (Go, Python)", "Fullstack-разработчик (Go, Python, FastAPI)", false},
		{"devsecops keeps devops family", "DevOps-инженер", "DevSecOps-инженер (CI/CD, безопасность)", false},
		// The observed drift: a backend title on the devops family conflicts.
		{"drift: backend title on devops base", "Middle DevOps-инженер Docker Kubernetes", "Middle Backend-разработчик (Go, Python, Rust): REST API (FastAPI), PostgreSQL, Docker, Linux", true},
		{"drift: devops title on backend base", "Backend-разработчик (Python)", "DevOps-инженер (Docker, Kubernetes)", true},
		{"drift: frontend title on fullstack base", "Fullstack-разработчик (Go, Python)", "Frontend-разработчик (React, TypeScript)", true},
		// Conservative: no tokens on either side → never a conflict.
		{"familyless base", "Менеджер продукта", "Backend-разработчик (Go)", false},
		{"familyless candidate", "DevOps-инженер", "Инженер (Go, Python)", false},
		{"familyless both", "Специалист", "Инженер широкого профиля", false},
		// Key skills contribute to the base anchor.
		{"base family from key skills", "Инженер DevOps практики CI/CD", "Backend-разработчик (Go)", true},
	}
	for _, tt := range tests {
		base, cand := titleFamilies(tt.base), titleFamilies(tt.candidate)
		if got := familyConflict(base, cand); got != tt.conflict {
			t.Fatalf("%s: conflict(%v, %v) = %v, want %v", tt.name, base, cand, got, tt.conflict)
		}
	}
}

func TestTitleFamiliesEmptyText(t *testing.T) {
	if got := titleFamilies(""); got != nil {
		t.Fatalf("empty text families = %v, want nil", got)
	}
}

// t93DevopsParent is the readable parent content: a devops-family base.
var t93DevopsParent = &hhapi.ResumeContent{
	Resume: map[string]json.RawMessage{
		"title":     json.RawMessage(`[{"string":"Middle DevOps-инженер"}]`),
		"keySkills": json.RawMessage(`[{"string":"Docker"},{"string":"Kubernetes"}]`),
	},
	Profile: map[string]json.RawMessage{},
}

// Clone path: the family anchor is the PARENT's positioning, so the drifted
// backend verdict on the devops family parks as needs_human (review, no
// apply) while the devops verdict passes.
func TestImproveTitleFamilyPostCheckClone(t *testing.T) {
	for _, tt := range []struct {
		verdict string
		status  string
	}{
		{verdict: "Middle Backend-разработчик (Go, Python)", status: StatusNeedsHuman},
		{verdict: "DevOps-инженер (CI/CD, Docker)", status: StatusImproved},
	} {
		ed := newFakeEditor(t)
		db := testStore(t)
		seedClone(t, db, "b4se123", "cloneF")
		rev := &fakeReviewer{}
		run := &t93Runner{verdicts: map[string]func() string{
			"title": improveVerdictFor(t, map[string]any{"value_string": tt.verdict}),
		}}
		im := newImprover(t, anyResumeCfg("title"), ed, run, db, rev)
		im.opt.ResumeContentFor = func(context.Context, string, string) (*hhapi.ResumeContent, error) {
			return t93DevopsParent, nil
		}
		artifact, err := im.ImproveClone(t.Context(), "acc1", "b4se123", "cloneF")
		if err != nil {
			t.Fatalf("%s: ImproveClone: %v", tt.verdict, err)
		}
		if artifact.Fields[0].Status != tt.status {
			t.Fatalf("%s: status = %q (%s), want %q", tt.verdict, artifact.Fields[0].Status, artifact.Fields[0].Reason, tt.status)
		}
		if tt.status == StatusNeedsHuman {
			if ed.applied {
				t.Fatal("a family switch must never auto-apply")
			}
			if len(rev.kinds) == 0 || rev.kinds[0] != ReviewKindImproveNeedsHuman {
				t.Fatalf("review kinds = %v, want %s", rev.kinds, ReviewKindImproveNeedsHuman)
			}
			if artifact.Fields[0].ReviewID == 0 {
				t.Fatal("review id not recorded on the diff field")
			}
		}
	}
}

// Base path: the anchor is the resume's own current positioning.
func TestImproveTitleFamilyPostCheckBase(t *testing.T) {
	ed := newFakeEditor(t) // fixture title: «DevOps-инженер» → the base family is devops
	db := testStore(t)
	rev := &fakeReviewer{}
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Backend-разработчик (Go, Python)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, rev)
	artifact, err := im.ImproveAnyResume(t.Context(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if artifact.Fields[0].Status != StatusNeedsHuman {
		t.Fatalf("status = %q (%s), want needs_human", artifact.Fields[0].Status, artifact.Fields[0].Reason)
	}
	if artifact.ParentHash != "" {
		t.Fatalf("base artifact carries a parent: %q", artifact.ParentHash)
	}
	if len(rev.kinds) != 1 {
		t.Fatalf("review kinds = %v", rev.kinds)
	}
}

// A failed parent read leaves NO anchor (never the clone's own drifted
// title): the same backend verdict passes ungated this run.
func TestImproveTitleFamilyCheckOffWithoutParent(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "b4se123", "cloneG")
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Backend-разработчик (Go, Python)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.ResumeContentFor = func(context.Context, string, string) (*hhapi.ResumeContent, error) {
		return nil, context.DeadlineExceeded
	}
	artifact, err := im.ImproveClone(t.Context(), "acc1", "b4se123", "cloneG")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if artifact.Fields[0].Status != StatusImproved {
		t.Fatalf("status = %q (%s), want improved (no anchor this run)", artifact.Fields[0].Status, artifact.Fields[0].Reason)
	}
}
