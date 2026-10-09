package autoclone

// T97: the create→improve chain anchors a fresh (created-from-scratch)
// shell on the REQUESTED --title. A fresh shell has no lineage and no own
// positioning, so without the anchor the title task's only substantive
// signal is the multi-role profile.md and the AI repositions on its
// dominant facts (the live defect: requested ИБ/QA/Преподаватель titles
// ignored). The anchor rides as the same «Позиционирование семьи» trusted
// fact the T93 lineage context uses, and the T93 (б) family post-check
// anchors on it the same way.

import (
	"context"
	"testing"

	"hhauto/internal/hhapi"
)

// The requested title rides EVERY task as the positioning fact — the same
// mechanics as the T93 parent fact (clone path covered there).
func TestImproveAnyResumePositioningFact(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Специалист по информационной безопасности"}),
		"about": improveVerdictFor(t, map[string]any{"unchanged": true}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, nil)
	im.opt.Positioning = "Специалист по информационной безопасности"

	if _, err := im.ImproveAnyResume(t.Context(), "acc1", "shell1"); err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	for field, in := range run.inputs {
		if !factsContain(in.TrustedFacts, "Позиционирование семьи: Специалист по информационной безопасности") {
			t.Fatalf("%s: positioning fact missing: %v", field, in.TrustedFacts)
		}
	}
}

// The post-check anchors on the REQUESTED title, not the shell's own
// (possibly absent or drifted) positioning: with the QA family requested,
// a devops-family verdict must park, though the shell fixture itself is
// devops-titled.
func TestImprovePositioningPostCheckAnchorsRequestedTitle(t *testing.T) {
	ed := newFakeEditor(t) // fixture title: «DevOps-инженер» → own family devops
	db := testStore(t)
	rev := &fakeReviewer{}
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (CI/CD, Docker)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, rev)
	im.opt.Positioning = "QA Automation Engineer (Python)"

	artifact, err := im.ImproveAnyResume(t.Context(), "acc1", "shell2")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if artifact.Fields[0].Status != StatusNeedsHuman {
		t.Fatalf("status = %q (%s), want needs_human (requested family qa, verdict devops)",
			artifact.Fields[0].Status, artifact.Fields[0].Reason)
	}
	if ed.applied {
		t.Fatal("a family switch against the requested title must never auto-apply")
	}
	if len(rev.kinds) != 1 || rev.kinds[0] != ReviewKindImproveNeedsHuman {
		t.Fatalf("review kinds = %v, want [%s]", rev.kinds, ReviewKindImproveNeedsHuman)
	}
}

// The check stays trivially safe for titles the conservative registry has
// no family tokens for (ИБ/Преподаватель): the verdict passes ungated —
// the requested-title-vs-itself check never blocks its own family.
func TestImprovePositioningFamilylessCheckOff(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Backend-разработчик (Go, Python)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.Positioning = "Специалист по информационной безопасности"

	artifact, err := im.ImproveAnyResume(t.Context(), "acc1", "shell3")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if artifact.Fields[0].Status != StatusImproved {
		t.Fatalf("status = %q (%s), want improved (no registered tokens on the requested family)",
			artifact.Fields[0].Status, artifact.Fields[0].Reason)
	}
}

// A readable lineage parent always wins over the requested positioning:
// the clone path keeps the T93 fact (never both).
func TestImproveCloneParentBeatsPositioningOption(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "b4se123", "cloneH")
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (Docker)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.Positioning = "QA Automation Engineer (Python)"
	im.opt.ResumeContentFor = func(context.Context, string, string) (*hhapi.ResumeContent, error) {
		return t93DevopsParent, nil
	}

	if _, err := im.ImproveClone(t.Context(), "acc1", "b4se123", "cloneH"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	facts := run.inputs["title"].TrustedFacts
	if !factsContain(facts, "Позиционирование семьи: Middle DevOps-инженер") {
		t.Fatalf("lineage fact missing: %v", facts)
	}
	if factsContainPrefix(facts, "Позиционирование семьи: QA") {
		t.Fatalf("positioning option must not ride the clone path: %v", facts)
	}
}
