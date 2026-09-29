package autoclone

// T57 key-skill level tests: the verdict-level parsing (backward-compatible
// extension of the improve verdict), the preserve/default policy over the
// captured userSkillLevels contract, and the pipeline e2e (levels PUT after
// the content apply, verify read-back, failure attribution).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
)

func TestKeySkillVerdictLevelsParsesEnum(t *testing.T) {
	verdict := json.RawMessage(`{
	  "value_strings": ["Linux","Docker","Rust"],
	  "key_skill_levels": [
	    {"name":"Rust","level":"Продвинутый"},
	    {"name":"  "},
	    {"name":"Docker","level":"гуру"},
	    {"name":"Docker","level":"Базовый"}
	  ]
	}`)
	got := keySkillVerdictLevels(verdict)
	if got[skillNameKey("Rust")] != "Продвинутый" {
		t.Fatalf("Rust level = %q", got[skillNameKey("Rust")])
	}
	// Garbage enum falls back to the default right away; a later duplicate
	// with a valid level wins (last write).
	if got[skillNameKey("Docker")] != "Базовый" {
		t.Fatalf("Docker level = %q, want the enum-normalized value", got[skillNameKey("Docker")])
	}
}

// THE COMPAT RULE: the legacy flat verdict (value_strings only) keeps
// working — every new skill then defaults to Средний.
func TestKeySkillVerdictLevelsFlatVerdictYieldsNil(t *testing.T) {
	for name, verdict := range map[string]json.RawMessage{
		"flat legacy":     json.RawMessage(`{"value_strings":["Linux","Docker"],"key_skill_levels":null}`),
		"absent key":      json.RawMessage(`{"value_strings":["Linux"]}`),
		"malformed array": json.RawMessage(`{"key_skill_levels":"Rust:Продвинутый"}`),
		"empty names":     json.RawMessage(`{"key_skill_levels":[{"name":"  ","level":"Базовый"}]}`),
	} {
		if got := keySkillVerdictLevels(verdict); got != nil {
			t.Fatalf("%s: levels = %v, want nil (caller defaults to Средний)", name, got)
		}
	}
}

func TestKeySkillLevelSetsPreservePolicy(t *testing.T) {
	skills := []hhapi.UserSkill{
		{ID: 1, Name: "Linux", LevelID: 10},         // already leveled — never sent
		{ID: 2, Name: "Docker", LevelID: 8},         // leveled, verdict says otherwise — preserve
		{ID: 3, Name: "Rust", LevelID: 0},           // new — verdict level
		{ID: 4, Name: "CrowdSec", LevelID: 0},       // new — absent in verdict → default
		{ID: 5, Name: "CI/CD Security", LevelID: 0}, // new — dash/space normalization on match
	}
	verdict := map[string]string{
		skillNameKey("Rust"):           "Продвинутый",
		skillNameKey("Docker"):         "Продвинутый", // must be IGNORED (preserve rule)
		skillNameKey("CI/CD Security"): "Базовый",
	}
	got := keySkillLevelSets(skills, verdict)
	want := []hhapi.KeySkillLevel{
		{SkillID: 3, LevelID: 10}, // Продвинутый
		{SkillID: 4, LevelID: 9},  // Средний (default)
		{SkillID: 5, LevelID: 8},  // Базовый
	}
	if len(got) != len(want) {
		t.Fatalf("sets = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("set[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A nil (flat legacy) verdict still levels every null-level skill —
	// with the default: this IS the backward-compat behavior.
	got = keySkillLevelSets(skills, nil)
	wantDefault := []hhapi.KeySkillLevel{
		{SkillID: 3, LevelID: 9},
		{SkillID: 4, LevelID: 9},
		{SkillID: 5, LevelID: 9},
	}
	if len(got) != len(wantDefault) {
		t.Fatalf("nil-verdict sets = %+v, want %+v", got, wantDefault)
	}
	for i := range wantDefault {
		if got[i] != wantDefault[i] {
			t.Fatalf("nil-verdict set[%d] = %+v, want %+v", i, got[i], wantDefault[i])
		}
	}
}

// fakeLevelEditor wraps the content fake with the T57 levels surface: the
// skill list is fixed; the level state mutates through SetKeySkillLevels,
// so the pipeline's verify read-back sees the applied levels.
type fakeLevelEditor struct {
	*fakeEditor
	mu      sync.Mutex
	skills  []hhapi.UserSkill
	levels  map[int64]int64
	setsLog [][]hhapi.KeySkillLevel
	setErr  error
}

func newFakeLevelEditor(t *testing.T, skills []hhapi.UserSkill) *fakeLevelEditor {
	t.Helper()
	levels := map[int64]int64{}
	for _, s := range skills {
		if s.LevelID != 0 {
			levels[s.ID] = s.LevelID
		}
	}
	return &fakeLevelEditor{fakeEditor: newFakeEditor(t), skills: skills, levels: levels}
}

func (e *fakeLevelEditor) UserSkills(_ context.Context, _ string) ([]hhapi.UserSkill, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]hhapi.UserSkill, len(e.skills))
	for i, s := range e.skills {
		out[i] = hhapi.UserSkill{ID: s.ID, Name: s.Name, LevelID: e.levels[s.ID]}
	}
	return out, nil
}

func (e *fakeLevelEditor) SetKeySkillLevels(_ context.Context, _ string, sets []hhapi.KeySkillLevel) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.setErr != nil {
		return e.setErr
	}
	copied := append([]hhapi.KeySkillLevel(nil), sets...)
	e.setsLog = append(e.setsLog, copied)
	for _, s := range sets {
		e.levels[s.SkillID] = s.LevelID
	}
	return nil
}

func (e *fakeLevelEditor) allSets() [][]hhapi.KeySkillLevel {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.setsLog
}

var _ keySkillLeveler = (*fakeLevelEditor)(nil)

func keySkillsCfg() config.AutoCloneImprove {
	return config.AutoCloneImprove{
		Enabled:      true,
		Fields:       []string{"title", "key_skills"},
		ModelProfile: "main",
	}
}

func keySkillsRunner(t *testing.T, levelsJSON string) *fakeImproveRunner {
	t.Helper()
	verdict := map[string]any{
		"value_strings": []string{"Linux", "Docker", "Rust"},
	}
	if levelsJSON != "" {
		verdict["key_skill_levels"] = json.RawMessage(levelsJSON)
	}
	return &fakeImproveRunner{verdicts: map[string]func() string{
		"title":      improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (платформа автоматизации)"}),
		"key_skills": improveVerdictFor(t, verdict),
	}}
}

func levelFixture() []hhapi.UserSkill {
	// Mirrors the live 2026-09-29 shape: leveled skills + one «не указан».
	return []hhapi.UserSkill{
		{ID: 1, Name: "Linux", LevelID: 10},
		{ID: 2, Name: "Docker", LevelID: 8},
		{ID: 3, Name: "Rust", LevelID: 0},
	}
}

func TestImproveAppliesKeySkillLevels(t *testing.T) {
	ed := newFakeLevelEditor(t, levelFixture())
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	run := keySkillsRunner(t, `[{"name":"Rust","level":"Продвинутый"},{"name":"Docker","level":"Продвинутый"}]`)
	im := newImprover(t, keySkillsCfg(), ed, run, db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	sets := ed.allSets()
	if len(sets) != 1 || len(sets[0]) != 1 || sets[0][0] != (hhapi.KeySkillLevel{SkillID: 3, LevelID: 10}) {
		t.Fatalf("levels PUTs = %+v, want exactly [Rust→Продвинутый(10)] (Docker preserved, Linux untouched)", sets)
	}
	if ks := fieldDiffByLocator(t, artifact, "resume.keySkills"); ks.Status != StatusImproved {
		t.Fatalf("key_skills status = %q (%s)", ks.Status, ks.Reason)
	}
	// Verify read-back: the fake state now carries the applied level.
	got, _ := ed.UserSkills(context.Background(), "cl0ne1")
	if got[2].LevelID != 10 {
		t.Fatalf("Rust level after apply = %d, want 10", got[2].LevelID)
	}
}

// THE COMPAT RULE, end to end: a flat legacy verdict still applies and every
// new skill defaults to Средний (captured level id 9).
func TestImproveKeySkillLevelsFlatVerdictDefaultsMiddle(t *testing.T) {
	ed := newFakeLevelEditor(t, levelFixture())
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	im := newImprover(t, keySkillsCfg(), ed, keySkillsRunner(t, ""), db, &fakeReviewer{})

	if _, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	sets := ed.allSets()
	if len(sets) != 1 || len(sets[0]) != 1 || sets[0][0] != (hhapi.KeySkillLevel{SkillID: 3, LevelID: 9}) {
		t.Fatalf("levels PUTs = %+v, want [Rust→Средний(9)]", sets)
	}
}

func TestImproveKeySkillLevelsFailureMarksField(t *testing.T) {
	ed := newFakeLevelEditor(t, levelFixture())
	ed.setErr = errors.New("hh refused the levels")
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	im := newImprover(t, keySkillsCfg(), ed, keySkillsRunner(t, `[{"name":"Rust","level":"Средний"}]`), db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v (a levels-step failure must not fail the run)", err)
	}
	ks := fieldDiffByLocator(t, artifact, "resume.keySkills")
	if ks.Status != StatusError || !strings.Contains(ks.Reason, "skill levels:") {
		t.Fatalf("key_skills = %q/%q, want error with the levels reason", ks.Status, ks.Reason)
	}
}

func TestImproveKeySkillLevelsSkipsWhenAllLeveled(t *testing.T) {
	skills := []hhapi.UserSkill{
		{ID: 1, Name: "Linux", LevelID: 10},
		{ID: 2, Name: "Docker", LevelID: 8},
		{ID: 3, Name: "Rust", LevelID: 9},
	}
	ed := newFakeLevelEditor(t, skills)
	db := testStore(t)
	seedClone(t, db, "p4rent", "cl0ne1")
	im := newImprover(t, keySkillsCfg(), ed, keySkillsRunner(t, `[{"name":"Rust","level":"Базовый"}]`), db, &fakeReviewer{})

	artifact, err := im.ImproveClone(context.Background(), "acc1", "p4rent", "cl0ne1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if sets := ed.allSets(); len(sets) != 0 {
		t.Fatalf("levels PUTs = %+v, want none (existing levels are preserved byte-for-byte)", sets)
	}
	if ks := fieldDiffByLocator(t, artifact, "resume.keySkills"); ks.Status != StatusImproved {
		t.Fatalf("key_skills status = %q", ks.Status)
	}
}
