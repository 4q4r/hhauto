package autoclone

// The T29 improve-flow scenarios (see improve_fakes_test.go for the fakes).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
)

// The happy path: both fields improve, one update POST carries the mutated
// split, the artifact records было/стало per field, the diff persists on
// the lineage row.
func TestImproveCloneAppliesFields(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	rev := &fakeReviewer{}
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (платформа автоматизации)"}),
		"about": improveVerdictFor(t, map[string]any{"value_string": "Автоматизация найма: CI и найм"}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, rev)

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 1 {
		t.Fatalf("update posts = %d, want 1", ed.updateCount())
	}
	if !ed.applied {
		t.Fatal("the fake hh must observe the applied payload")
	}
	title := fieldDiffByLocator(t, artifact, "resume.title")
	if title.Status != StatusImproved {
		t.Fatalf("title status = %q", title.Status)
	}
	if string(title.Before) != `"DevOps-инженер"` {
		t.Fatalf("title before = %s", title.Before)
	}
	if string(title.After) != `"DevOps-инженер (платформа автоматизации)"` {
		t.Fatalf("title after = %s", title.After)
	}
	if about := fieldDiffByLocator(t, artifact, "resume.skills"); about.Status != StatusImproved {
		t.Fatalf("about status = %q (%s)", about.Status, about.Reason)
	}
	// Artifact persisted on the lineage row.
	row, err := db.ResumeClonesGet(context.Background(), "cl0ne1")
	if err != nil || row == nil {
		t.Fatalf("ResumeClonesGet: %v (%v)", row, err)
	}
	if !strings.Contains(row.ImproveDiff, `"locator":"resume.title"`) {
		t.Fatalf("stored diff = %s", row.ImproveDiff)
	}
	if rev.kinds != nil {
		t.Fatalf("no review expected on the happy path: %v", rev.kinds)
	}
}

// THE INVARIANT: a hash that is not a tracked clone of the given parent
// (a base resume!) is never mutated.
func TestImproveCloneRejectsBaseResume(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	im := newImprover(t, baseImproveCfg(), ed, defaultRunner(t, "title", "Новый"), db, &fakeReviewer{})

	// Unknown hash.
	if _, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "b4se_resume"); !errors.Is(err, ErrNotClone) {
		t.Fatalf("unknown hash err = %v, want ErrNotClone", err)
	}
	// Parent mismatch: the clone belongs to another lineage.
	if _, err := im.ImproveClone(context.Background(), "acc1", "other_parent", "cl0ne1"); !errors.Is(err, ErrNotClone) {
		t.Fatalf("parent mismatch err = %v, want ErrNotClone", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("a non-clone resume must never be posted to the update endpoint")
	}
}

// Dry-run: the plan and the diff are produced, nothing is applied and
// nothing is persisted as applied.
func TestImproveDryRunNoPosts(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	cfg := baseImproveCfg()
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	im, err := New(Options{
		API: ed, Runner: defaultRunner(t, "title", "Заголовок из dry-run"), DB: db,
		Registry: reg, Cfg: cfg, DryRun: true, Log: testLoggerT(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("dry-run must not post any update")
	}
	if !artifact.DryRun || artifact.Applied {
		t.Fatalf("artifact dry_run/applied = %t/%t", artifact.DryRun, artifact.Applied)
	}
	title := fieldDiffByLocator(t, artifact, "resume.title")
	if title.Status != StatusImproved || string(title.After) != `"Заголовок из dry-run"` {
		t.Fatalf("dry-run title diff = %+v", title)
	}
}

// An invented number in the improved text (needs_human verdict from the
// deterministic post-checks) skips the field and parks a review.
func TestImproveNeedsHumanSkipsField(t *testing.T) {
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

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("a needs_human field must not be applied")
	}
	title := fieldDiffByLocator(t, artifact, "resume.title")
	if title.Status != StatusNeedsHuman {
		t.Fatalf("title status = %q", title.Status)
	}
	if len(rev.kinds) != 1 || rev.kinds[0] != ReviewKindImproveNeedsHuman {
		t.Fatalf("review kinds = %v", rev.kinds)
	}
}

// always_ask fields never auto-apply: the proposed diff parks for review
// while the other fields still apply.
func TestImproveAlwaysAskParksForReview(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	rev := &fakeReviewer{}
	cfg := baseImproveCfg()
	cfg.AlwaysAsk = []string{"about"}
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Заголовок ок"}),
		"about": improveVerdictFor(t, map[string]any{"value_string": "Текст на ревью"}),
	}}
	im := newImprover(t, cfg, ed, run, db, rev)

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 1 {
		t.Fatalf("update posts = %d, want 1 (only the non-ask field)", ed.updateCount())
	}
	if s := condStringsT(t, ed.state.Resume["title"]); len(s) != 1 || s[0] != "Заголовок ок" {
		t.Fatalf("applied title = %v", s)
	}
	ask := fieldDiffByLocator(t, artifact, "resume.skills")
	if ask.Status != StatusAlwaysAsk {
		t.Fatalf("about status = %q", ask.Status)
	}
	if !strings.Contains(string(ed.state.Resume["skills"]), "Автоматизация найма") {
		t.Fatal("always_ask value must not land in the payload")
	}
	if len(rev.kinds) != 1 || rev.kinds[0] != ReviewKindImproveAlwaysAsk {
		t.Fatalf("review kinds = %v", rev.kinds)
	}
}

// hh validation rejection: one retry with a normalized value; a persistent
// failure skips the field (never aborts the whole apply).
func TestImproveValidationRetryOnce(t *testing.T) {
	ed := newFakeEditor(t)
	ed.failCodes = [][]string{{"title:MAXLENGTH"}}
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Короткий заголовок"}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 2 {
		t.Fatalf("update posts = %d, want 2 (original + one retry)", ed.updateCount())
	}
	title := fieldDiffByLocator(t, artifact, "resume.title")
	if title.Status != StatusImproved {
		t.Fatalf("title status = %q (%s)", title.Status, title.Reason)
	}
}

// A field that fails validation twice is skipped: the other field still
// applies.
func TestImproveValidationTwiceSkipsField(t *testing.T) {
	ed := newFakeEditor(t)
	ed.failCodes = [][]string{{"title:MAXLENGTH"}, {"title:MAXLENGTH"}}
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Заголовок А"}),
		"about": improveVerdictFor(t, map[string]any{"value_string": "Текст Б"}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 2 {
		t.Fatalf("update posts = %d, want 2 (retry then drop the field)", ed.updateCount())
	}
	title := fieldDiffByLocator(t, artifact, "resume.title")
	if title.Status != StatusValidationFailed {
		t.Fatalf("title status = %q", title.Status)
	}
	about := fieldDiffByLocator(t, artifact, "resume.skills")
	if about.Status != StatusImproved {
		t.Fatalf("about status = %q", about.Status)
	}
	if !strings.Contains(string(ed.state.Resume["skills"]), "Текст Б") {
		t.Fatalf("applied state = %s", ed.state.Resume["skills"])
	}
	if strings.Contains(string(ed.state.Resume["title"]), "Заголовок А") {
		t.Fatal("the failed field must not land in the final payload")
	}
}

// Runner failures skip the field, produce an artifact, and never mutate.
func TestImproveRunnerErrorSkipsField(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	run := &fakeImproveRunner{errs: map[string]error{"title": errors.New("provider down")}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("no updates when every field fails")
	}
	if s := fieldDiffByLocator(t, artifact, "resume.title").Status; s != StatusError {
		t.Fatalf("title status = %q", s)
	}
}

// unchanged verdicts are recorded but do not post.
func TestImproveUnchangedSkipsPost(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"unchanged": true}),
	}}
	im := newImprover(t, baseImproveCfg(), ed, run, db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("unchanged must not post")
	}
	if s := fieldDiffByLocator(t, artifact, "resume.title").Status; s != StatusUnchanged {
		t.Fatalf("title status = %q", s)
	}
}

// key_skills (list slot) applies; salary lands with the currency riding
// from the current value.
func TestImproveListAndSalaryFields(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	cfg := config.AutoCloneImprove{
		Enabled: true, Fields: []string{"key_skills", "salary"}, ModelProfile: "main",
	}
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"key_skills": improveVerdictFor(t, map[string]any{"value_strings": []string{"Linux", "Docker", "Kubernetes"}}),
		"salary":     improveVerdictFor(t, map[string]any{"value_amount": 180000}),
	}}
	im := newImprover(t, cfg, ed, run, db, &fakeReviewer{})

	if _, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 1 {
		t.Fatalf("update posts = %d, want 1 (both fields ride one payload)", ed.updateCount())
	}
	var skills []map[string]string
	if err := json.Unmarshal(ed.state.Resume["keySkills"], &skills); err != nil || len(skills) != 3 {
		t.Fatalf("applied keySkills = %s", ed.state.Resume["keySkills"])
	}
	var salary []map[string]any
	if err := json.Unmarshal(ed.state.Resume["salary"], &salary); err != nil || salary[0]["amount"].(float64) != 180000 || salary[0]["currency"] != "RUR" {
		t.Fatalf("applied salary = %s", ed.state.Resume["salary"])
	}
}

// experience_description expands per item: two items, two targets, both
// applied in one payload.
func TestImproveExperienceDescriptionPerItem(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	cfg := config.AutoCloneImprove{Enabled: true, Fields: []string{"experience_description"}, ModelProfile: "main"}
	var calls int
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"experience_description": func() string {
			calls++
			return improveVerdictFor(t, map[string]any{"value_string": "Усиленное описание " + string(rune('0'+calls))})( /* regenerated per call */ )
		},
	}}
	// The fake regenerates the verdict per call: patch it to vary by item.
	run.verdicts["experience_description"] = func() string {
		calls++
		m := map[string]any{"value_string": "Усиленное описание " + string(rune('0'+calls))}
		return improveVerdictFor(t, m)()
	}
	im := newImprover(t, cfg, ed, run, db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if calls != 2 {
		t.Fatalf("AI calls = %d, want 2 (one per item)", calls)
	}
	if ed.updateCount() != 1 {
		t.Fatalf("update posts = %d, want 1", ed.updateCount())
	}
	if d := fieldDiffByLocator(t, artifact, "profile.experience.0.description"); d.Status != StatusImproved {
		t.Fatalf("item0 status = %q", d.Status)
	}
	if d := fieldDiffByLocator(t, artifact, "profile.experience.1.description"); d.Status != StatusImproved {
		t.Fatalf("item1 status = %q", d.Status)
	}
	var items []map[string]any
	if err := json.Unmarshal(ed.state.Profile["experience"], &items); err != nil {
		t.Fatalf("decode applied experience: %v", err)
	}
	if items[0]["description"] != "Усиленное описание 1" || items[1]["description"] != "Усиленное описание 2" {
		t.Fatalf("applied descriptions = %v/%v", items[0]["description"], items[1]["description"])
	}
}

// The effective system prompt rides the per-field registry (builtin or
// override) — the runner receives base+field in one system string.
func TestImproveSendsFieldPrompt(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	cfg := baseImproveCfg()
	cfg.Prompts = map[string]config.AITask{"title": {PromptInline: "Свой промпт титула"}}
	run := &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "X заголовок"}),
	}}
	im := newImprover(t, cfg, ed, run, db, &fakeReviewer{})
	if _, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if run.systems["title"] != "Свой промпт титула" {
		t.Fatalf("system prompt = %q", run.systems["title"])
	}
}

// Wiring: improve enabled without a runner fails fast.
func TestNewImproverRequiresRunner(t *testing.T) {
	reg, err := NewPromptRegistry(baseImproveCfg())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if _, err := New(Options{Registry: reg, Cfg: baseImproveCfg(), DB: testStore(t), Log: testLoggerT()}); err == nil {
		t.Fatal("improve without a runner must fail at wiring")
	}
}

func condStringsT(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var out []string
	for _, p := range parts {
		var s string
		if err := json.Unmarshal(p["string"], &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

var (
	_ = hhapi.ErrCloneMaterialize
	_ = ai.TaskResumeImprove
)
