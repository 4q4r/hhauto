package autoclone

// T93 lineage-awareness tests: shard edge resolution, auto-registration on
// improve, the parent context facts (positioning on every task; the
// parent's same-field value only when it differs) and the safe degradation
// when the parent content cannot be read (no anchor at all — never the
// clone's own possibly-drifted title).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// t93Runner captures the ImproveInput of every field call and scripts
// verdicts per field.
type t93Runner struct {
	verdicts map[string]func() string
	inputs   map[string]ai.ImproveInput
}

func (r *t93Runner) Run(context.Context, string, string, string, string, ai.TaskName, any) (json.RawMessage, ai.Decision, error) {
	return nil, ai.Decision{}, errors.New("not used in T93 tests")
}

func (r *t93Runner) RunWithSystem(_ context.Context, _, _, _, _ string, _ ai.TaskName, _ string, input any) (json.RawMessage, ai.Decision, error) {
	in, ok := input.(ai.ImproveInput)
	if !ok {
		return nil, ai.Decision{}, errors.New("unexpected input type")
	}
	if r.inputs == nil {
		r.inputs = map[string]ai.ImproveInput{}
	}
	r.inputs[in.Field] = in
	gen, ok := r.verdicts[in.Field]
	if !ok {
		return nil, ai.Decision{}, errors.New("no scripted verdict for " + in.Field)
	}
	return json.RawMessage(gen()), ai.Decision{Task: ai.TaskResumeImprove}, nil
}

// t93ContentFor serves parent content by hash (the T93 surface).
type t93ContentFor map[string]*hhapi.ResumeContent

func (c t93ContentFor) For(_ context.Context, _, hash string) (*hhapi.ResumeContent, error) {
	if cc, ok := c[hash]; ok {
		return cc, nil
	}
	return nil, fmt.Errorf("no content for %s", hash)
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestLineageEdgesFromResumes(t *testing.T) {
	resumes := []hhapi.Resume{
		{HHID: "clone1", Raw: mustRaw(t, map[string]any{"_attributes": map[string]any{"id": "100", "hash": "clone1", "parentResumeId": "200"}})},
		{HHID: "parent1", Raw: mustRaw(t, map[string]any{"_attributes": map[string]any{"id": "200", "hash": "parent1"}})},
		{HHID: "clone2", Raw: mustRaw(t, map[string]any{"_attributes": map[string]any{"id": 300, "hash": "clone2", "parentResumeId": "200"}})},     // numeric id shape
		{HHID: "orphan", Raw: mustRaw(t, map[string]any{"_attributes": map[string]any{"id": "400", "hash": "orphan", "parentResumeId": "999"}})},   // parent unknown
		{HHID: "selfish", Raw: mustRaw(t, map[string]any{"_attributes": map[string]any{"id": "500", "hash": "selfish", "parentResumeId": "500"}})}, // self-parent
	}
	edges := LineageEdges(resumes)
	if len(edges) != 2 {
		t.Fatalf("edges = %+v, want clone1+clone2 → parent1", edges)
	}
	got := map[string]string{}
	for _, e := range edges {
		got[e.Clone] = e.Parent
	}
	if got["clone1"] != "parent1" || got["clone2"] != "parent1" {
		t.Fatalf("edges = %+v", got)
	}
}

// Auto-registration: an untracked clone with a shard lineage edge in the
// account snapshots registers on improve (idempotent — a second run adds
// nothing) and the artifact carries the resolved parent.
func TestImproveAnyResumeAutoRegistersLineage(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	if err := db.AccountsUpsert(t.Context(), store.Account{ID: "acc1"}); err != nil {
		t.Fatalf("account: %v", err)
	}
	parentRaw := `{"_attributes":{"id":"200","hash":"b4se123","parentResumeId":null}}`
	cloneRaw := `{"_attributes":{"id":"100","hash":"cloneX","parentResumeId":"200"}}`
	for _, r := range []store.Resume{
		{AccountID: "acc1", HhResumeID: "b4se123", Raw: parentRaw, UpdatedAt: time.Now()},
		{AccountID: "acc1", HhResumeID: "cloneX", Raw: cloneRaw, UpdatedAt: time.Now()},
	} {
		if err := db.ResumesUpsert(t.Context(), &r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (Docker)"}),
	}}
	cfg := anyResumeCfg("title")
	im := newImprover(t, cfg, ed, run, db, nil)
	im.opt.DryRun = true

	artifact, err := im.ImproveAnyResume(t.Context(), "acc1", "cloneX")
	if err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	if artifact.ParentHash != "b4se123" {
		t.Fatalf("artifact.ParentHash = %q, want b4se123", artifact.ParentHash)
	}
	row, err := db.ResumeClonesGet(t.Context(), "cloneX")
	if err != nil || row == nil {
		t.Fatalf("clone not registered: %v %v", row, err)
	}
	if row.ParentHash != "b4se123" || row.Depth != 1 || row.AccountID != "acc1" {
		t.Fatalf("lineage row = %+v", row)
	}
	// Idempotent: the second run must not add a row.
	if _, err := im.ImproveAnyResume(t.Context(), "acc1", "cloneX"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	rows, err := db.ResumeClonesListByAccount(t.Context(), "acc1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after second run = %d (%v), want 1", len(rows), err)
	}
}

// The parent context: the positioning fact rides EVERY task (title and
// about); the parent's same-field value rides only when it differs from
// the clone's value.
func TestImproveCloneParentContextFacts(t *testing.T) {
	ed := newFakeEditor(t) // clone: title "DevOps-инженер", skills "Автоматизация найма"
	db := testStore(t)
	seedClone(t, db, "b4se123", "cloneY")
	parentContent := &hhapi.ResumeContent{
		Resume: map[string]json.RawMessage{
			"title":  json.RawMessage(`[{"string":"Middle DevOps-инженер"}]`), // differs from the clone's title
			"skills": json.RawMessage(`[{"string":"Автоматизация найма"}]`),   // identical to the clone's about
		},
		Profile: map[string]json.RawMessage{},
	}
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (CI/CD, Docker)"}),
		"about": improveVerdictFor(t, map[string]any{"unchanged": true}),
	}}
	cfg := baseImproveCfg() // title + about
	im := newImprover(t, cfg, ed, run, db, nil)
	im.opt.ResumeContentFor = t93ContentFor{"b4se123": parentContent}.For

	if _, err := im.ImproveClone(t.Context(), "acc1", "b4se123", "cloneY"); err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	titleIn, ok := run.inputs["title"]
	if !ok {
		t.Fatal("no title call captured")
	}
	aboutIn := run.inputs["about"]
	for field, in := range map[string]ai.ImproveInput{"title": titleIn, "about": aboutIn} {
		if !factsContain(in.TrustedFacts, "Позиционирование семьи: Middle DevOps-инженер") {
			t.Fatalf("%s: positioning fact missing: %v", field, in.TrustedFacts)
		}
	}
	if !factsContainPrefix(titleIn.TrustedFacts, "Значение родителя (то же поле базового резюме): ") {
		t.Fatalf("title: parent-value fact missing (values differ): %v", titleIn.TrustedFacts)
	}
	if factsContainPrefix(aboutIn.TrustedFacts, "Значение родителя") {
		t.Fatalf("about: parent-value fact must be absent (identical values): %v", aboutIn.TrustedFacts)
	}
}

// A failed parent read degrades to NO lineage facts at all — never an
// anchor on the clone's own possibly-drifted title.
func TestImproveParentFetchFailureDegrades(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "b4se123", "cloneZ")
	run := &t93Runner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "DevOps-инженер (Kubernetes)"}),
	}}
	im := newImprover(t, anyResumeCfg("title"), ed, run, db, nil)
	im.opt.ResumeContentFor = func(context.Context, string, string) (*hhapi.ResumeContent, error) {
		return nil, errors.New("hh down")
	}
	artifact, err := im.ImproveClone(t.Context(), "acc1", "b4se123", "cloneZ")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if artifact.Fields[0].Status != StatusImproved {
		t.Fatalf("title status = %q (%s)", artifact.Fields[0].Status, artifact.Fields[0].Reason)
	}
	facts := run.inputs["title"].TrustedFacts
	if factsContainPrefix(facts, "Позиционирование семьи:") || factsContainPrefix(facts, "Значение родителя") {
		t.Fatalf("lineage facts must be absent on parent-fetch failure: %v", facts)
	}
}

func factsContain(facts []string, want string) bool {
	for _, f := range facts {
		if f == want {
			return true
		}
	}
	return false
}

func factsContainPrefix(facts []string, prefix string) bool {
	for _, f := range facts {
		if len(f) >= len(prefix) && f[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
