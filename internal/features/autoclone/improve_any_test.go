package autoclone

// T35 improve-any-resume: the T29 pipeline over ANY resume hash (the
// standalone `hhauto resume improve` command), not only tracked clones.
// The base-resume invariant moves up into the CLI: a base hash improves
// only with the explicit --confirm flag, which this package cannot see —
// ImproveAnyResume is the confirmed path, ImproveClone stays strict.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hhauto/internal/config"
)

// fakePlanEditor implements EditorRunner with scripted outcomes.
type fakePlanEditor struct {
	// err is returned verbatim from ApplyFieldPlan (ErrEditorUnavailable
	// scripts the degradation class).
	err error
	// fail marks locators that did not land.
	fail map[string]string
	// appliedPlan captures the last plan.
	appliedPlan []FieldChange
	appliedHash string
}

func (e *fakePlanEditor) ApplyFieldPlan(_ context.Context, hash string, plan []FieldChange) (ApplyResult, error) {
	if e.err != nil {
		return ApplyResult{}, e.err
	}
	e.appliedHash = hash
	e.appliedPlan = append([]FieldChange(nil), plan...)
	res := ApplyResult{}
	for _, fc := range plan {
		if reason, bad := e.fail[fc.Locator]; bad {
			res.Failed = append(res.Failed, FieldApplyError{Field: fc.Locator, Err: reason})
			continue
		}
		res.Applied = append(res.Applied, fc.Locator)
	}
	return res, nil
}

func anyResumeCfg(fields ...string) config.AutoCloneImprove {
	cfg := baseImproveCfg()
	if len(fields) > 0 {
		cfg.Fields = fields
	}
	return cfg
}

// Dry run: the diff is computed было/стало, nothing is posted, nothing is
// applied and no editor/browser runs.
func TestImproveAnyResumeDryRun(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := defaultRunner(t, "title", "Улучшенный заголовок")
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.DryRun = true

	artifact, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if !artifact.DryRun || artifact.Applied {
		t.Fatalf("dry-run artifact flags: dry_run=%v applied=%v", artifact.DryRun, artifact.Applied)
	}
	if ed.updateCount() != 0 || ed.applied {
		t.Fatal("dry run must not post updates")
	}
	title := fieldDiffByLocator(t, artifact, "resume.title")
	if title.Status != StatusImproved {
		t.Fatalf("title status = %q (%s)", title.Status, title.Reason)
	}
	if string(title.Before) != `"DevOps-инженер"` || string(title.After) != `"Улучшенный заголовок"` {
		t.Fatalf("was/stale = %s / %s", title.Before, title.After)
	}
	// Base resume: no lineage row, nothing persisted anywhere.
	row, _ := db.ResumeClonesGet(context.Background(), "b4se123")
	if row != nil {
		t.Fatalf("base resume must not gain a lineage row: %+v", row)
	}
}

// The happy path on a BASE resume: the browser editor applies the plan and
// the artifact reports improved fields; the store gains no lineage row.
func TestImproveAnyResumeAppliesViaEditor(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := defaultRunner(t, "title", "DevOps-инженер (автоматизация)")
	editor := &fakePlanEditor{}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.Editor = editor

	artifact, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if editor.appliedHash != "b4se123" || len(editor.appliedPlan) != 1 {
		t.Fatalf("editor plan not applied as scripted: %q %v", editor.appliedHash, editor.appliedPlan)
	}
	if !artifact.Applied {
		t.Fatal("artifact.Applied must be true after a successful editor apply")
	}
	if f := fieldDiffByLocator(t, artifact, "resume.title"); f.Status != StatusImproved {
		t.Fatalf("title status = %q", f.Status)
	}
}

// Degradation: the browser editor is unavailable (Chrome missing etc.) —
// the planned fields flip to error and the whole plan parks for manual
// apply, exactly like the clone path.
func TestImproveAnyResumeEditorUnavailableParksReview(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	rev := &fakeReviewer{}
	run := defaultRunner(t, "title", "DevOps-инженер (автоматизация)")
	editor := &fakePlanEditor{err: ErrChromeMissingEditor}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, rev)
	im.opt.Editor = editor

	artifact, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("degradation must not error: %v", err)
	}
	if f := fieldDiffByLocator(t, artifact, "resume.title"); f.Status != StatusError {
		t.Fatalf("title status = %q, want error", f.Status)
	}
	if len(rev.kinds) != 1 || rev.kinds[0] != ReviewKindImproveNeedsHuman {
		t.Fatalf("review kinds = %v, want one %s", rev.kinds, ReviewKindImproveNeedsHuman)
	}
	if ed.updateCount() != 0 {
		t.Fatal("the API fallback must not fire when a degradation is parked")
	}
}

// Without an editor the plan applies through the builder-update API path
// (the T29 applyPending semantics, including the artifact statuses).
func TestImproveAnyResumeAppliesViaAPIUpdate(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := defaultRunner(t, "title", "DevOps-инженер (API-путь)")
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)

	artifact, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if ed.updateCount() != 1 || !ed.applied {
		t.Fatalf("api update posts = %d applied=%v", ed.updateCount(), ed.applied)
	}
	if !artifact.Applied {
		t.Fatal("artifact.Applied must be true after a successful API apply")
	}
}

// A hash that IS a tracked clone keeps the full clone contract: the diff
// persists on the lineage row.
func TestImproveAnyResumePersistsOnTrackedClone(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne35")
	run := defaultRunner(t, "title", "Заголовок клона")
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)

	if _, err := im.ImproveAnyResume(context.Background(), "acc1", "cl0ne35"); err != nil {
		t.Fatalf("ImproveAnyResume on clone: %v", err)
	}
	row, err := db.ResumeClonesGet(context.Background(), "cl0ne35")
	if err != nil || row == nil {
		t.Fatalf("ResumeClonesGet: %v %v", row, err)
	}
	if !strings.Contains(row.ImproveDiff, `"locator":"resume.title"`) {
		t.Fatalf("stored diff = %q", row.ImproveDiff)
	}
}

// The invariant difference in one place: ImproveClone still refuses base
// resumes; ImproveAnyResume is the confirmed escape hatch.
func TestImproveAnyResumeVsCloneInvariant(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := defaultRunner(t, "title", "Одинаково для обоих путей")
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)

	if _, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "b4se123"); !errors.Is(err, ErrNotClone) {
		t.Fatalf("ImproveClone on base = %v, want ErrNotClone", err)
	}
	if _, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123"); err != nil {
		t.Fatalf("ImproveAnyResume on base must work: %v", err)
	}
}

// Per-field failures degrade to skipped/error statuses and never abort the
// whole run (shared semantics with the clone path, pinned for the command).
func TestImproveAnyResumeFieldErrorDegrades(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	run := &fakeImproveRunner{
		verdicts: map[string]func() string{
			"title": improveVerdictFor(t, map[string]any{"value_string": "Готовый заголовок"}),
			"about": improveVerdictFor(t, map[string]any{"needs_human": true, "needs_human_reason": "мало фактов"}),
		},
	}
	im := newImprover(t, anyResumeCfg("title", "about"), ed, run, db, nil)

	artifact, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if f := fieldDiffByLocator(t, artifact, "resume.title"); f.Status != StatusImproved {
		t.Fatalf("title = %q", f.Status)
	}
	if f := fieldDiffByLocator(t, artifact, "resume.skills"); f.Status != StatusNeedsHuman {
		t.Fatalf("about = %q (%s)", f.Status, f.Reason)
	}
	if !artifact.Applied {
		t.Fatal("the improved title must mark the artifact applied (api path)")
	}
}
