package autoclone

// T29 improve-flow tests: the planner→apply→verify pipeline with fakes for
// the editor surface, the AI runner, the review queue and the store. The
// tests pin the owner invariants: base resumes are never mutated, facts
// are never invented (needs_human skips), always_ask parks for review,
// dry-run posts nothing, validation errors retry once and then skip.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/review"
	"hhauto/internal/store"
)

// --- editor fake ------------------------------------------------------------

// fakeEditor implements hhapi.ResumeContentEditor with a stateful fixture.
type fakeEditor struct {
	mu      sync.Mutex
	state   contentState
	bodies  []string // captured update payload titles
	applied bool
	// script: each entry fires once as a validation rejection before the
	// update succeeds.
	failCodes [][]string
}

// contentState mirrors the builder-state split the editor surface trades.
type contentState struct {
	Resume     map[string]json.RawMessage
	Profile    map[string]json.RawMessage
	Screens    []string
	Conditions map[string]json.RawMessage
}

func newFakeEditor(t *testing.T) *fakeEditor {
	t.Helper()
	var st contentState
	if err := json.Unmarshal([]byte(contentFixtureT29()), &st); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	st.Resume = cloneRawMap(st.Resume)
	st.Profile = cloneRawMap(st.Profile)
	return &fakeEditor{state: st}
}

func contentFixtureT29() string {
	return `{
	  "screens": ["title", "key_skills", "about", "experience"],
	  "resume": {
	    "title": [{"string":"DevOps-инженер"}],
	    "skills": [{"string":"Автоматизация найма"}],
	    "keySkills": [{"string":"Linux"},{"string":"Docker"}],
	    "salary": [{"amount":0,"currency":"RUR"}]
	  },
	  "profile": {
	    "experience": [
	      {"companyName":"Альфа","position":"Инженер","description":"Занимался CI. Внедрил пайплайны.","startDate":"2020-01-01","endDate":"2022-05-01"},
	      {"companyName":"Бета","position":"Старший инженер","description":"Занимался CI/CD и облаком.","startDate":"2022-06-01","endDate":null}
	    ]
	  }
	}`
}

func cloneRawMap(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *contentState) toResumeContent() *hhapi.ResumeContent {
	return &hhapi.ResumeContent{
		Resume:     s.Resume,
		Profile:    s.Profile,
		Screens:    append([]string(nil), s.Screens...),
		Conditions: cloneRawMap(s.Conditions),
	}
}

func (e *fakeEditor) ResumeContent(_ context.Context, _ string) (*hhapi.ResumeContent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.toResumeContent(), nil
}

func (e *fakeEditor) UpdateResumeContent(_ context.Context, _ string, content *hhapi.ResumeContent, _ []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bodies = append(e.bodies, string(content.Resume["title"]))
	if n := len(e.failCodes); n > 0 {
		codes := e.failCodes[0]
		e.failCodes = e.failCodes[1:]
		// hh validates per field: the non-named fields land even on a
		// rejected POST. Apply the payload minus the errored keys.
		errKeys := map[string]bool{}
		for _, c := range codes {
			if i := indexByte(c, ':'); i >= 0 {
				c = c[:i]
			}
			errKeys[c] = true
		}
		e.applyPartial(content, errKeys)
		return &hhapi.ResumeUpdateValidationError{Codes: codes, Err: hhapi.ErrCloneMaterialize}
	}
	// hh applies: the state becomes the payload the verify read-back sees.
	e.state.Resume = content.Resume
	e.state.Profile = content.Profile
	e.applied = true
	return nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// applyPartial copies the payload into the state skipping the errored keys.
func (e *fakeEditor) applyPartial(content *hhapi.ResumeContent, errKeys map[string]bool) {
	next := map[string]json.RawMessage{}
	for k, v := range content.Resume {
		if !errKeys[k] {
			next[k] = v
			continue
		}
		if cur, ok := e.state.Resume[k]; ok {
			next[k] = cur
		}
	}
	e.state.Resume = next
	nextP := map[string]json.RawMessage{}
	for k, v := range content.Profile {
		nextP[k] = v
	}
	e.state.Profile = nextP
	e.applied = true
}

func (e *fakeEditor) updateCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.bodies)
}

// --- runner fake ------------------------------------------------------------

type fakeImproveRunner struct {
	verdicts map[string]func() string // field id → raw verdict JSON
	errs     map[string]error
	systems  map[string]string
}

func (r *fakeImproveRunner) Run(context.Context, string, string, string, string, ai.TaskName, any) (json.RawMessage, ai.Decision, error) {
	return nil, ai.Decision{}, errors.New("not used in T29 tests")
}

func (r *fakeImproveRunner) RunWithSystem(_ context.Context, _, _, _, _ string, _ ai.TaskName, system string, input any) (json.RawMessage, ai.Decision, error) {
	in, ok := input.(ai.ImproveInput)
	if !ok {
		return nil, ai.Decision{}, errors.New("unexpected input type")
	}
	if r.systems == nil {
		r.systems = map[string]string{}
	}
	r.systems[in.Field] = system
	if err, ok := r.errs[in.Field]; ok {
		return nil, ai.Decision{}, err
	}
	gen, ok := r.verdicts[in.Field]
	if !ok {
		return nil, ai.Decision{}, errors.New("no scripted verdict for " + in.Field)
	}
	return json.RawMessage(gen()), ai.Decision{Task: ai.TaskResumeImprove}, nil
}

func improveVerdictFor(t *testing.T, m map[string]any) func() string {
	t.Helper()
	if _, ok := m["confidence"]; !ok {
		m["confidence"] = 0.9
	}
	if _, ok := m["needs_human"]; !ok {
		m["needs_human"] = false
	}
	if _, ok := m["needs_human_reason"]; !ok {
		m["needs_human_reason"] = nil
	}
	if _, ok := m["injection_suspected"]; !ok {
		m["injection_suspected"] = false
	}
	if _, ok := m["unchanged"]; !ok {
		m["unchanged"] = false
	}
	if _, ok := m["used_facts"]; !ok {
		m["used_facts"] = []string{}
	}
	if _, ok := m["missing_data"]; !ok {
		m["missing_data"] = []string{}
	}
	for _, slot := range []string{"value_string", "value_strings", "value_amount"} {
		if _, ok := m[slot]; !ok {
			m[slot] = nil
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	return func() string { return string(raw) }
}

// --- store/review fakes -----------------------------------------------------

func testStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeReviewer struct {
	mu       sync.Mutex
	kinds    []string
	payloads []string
}

func (r *fakeReviewer) Add(_ context.Context, _, kind, _, payload string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kind)
	r.payloads = append(r.payloads, payload)
	return int64(len(r.kinds)), nil
}

var _ review.Reviewer = (*fakeReviewer)(nil)

// --- helpers ----------------------------------------------------------------

func baseImproveCfg() config.AutoCloneImprove {
	return config.AutoCloneImprove{
		Enabled:      true,
		Fields:       []string{"title", "about"},
		ModelProfile: "main",
	}
}

func newImprover(t *testing.T, cfg config.AutoCloneImprove, ed hhapi.ResumeContentEditor, run ai.FieldPromptRunner, db *store.DB, rev review.Reviewer) *Improver {
	t.Helper()
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	im, err := New(Options{
		API: ed, Runner: run, DB: db, Review: rev, Registry: reg,
		Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:            func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
		InterCallPause: time.Millisecond, // T41: tests must not sleep the 2s production pause
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return im
}

func seedClone(t *testing.T, db *store.DB, parent, clone string) {
	t.Helper()
	if err := db.ResumeClonesRegister(context.Background(), &store.ResumeClone{
		CloneHash: clone, ParentHash: parent, Depth: 1, AccountID: "acc1",
	}); err != nil {
		t.Fatalf("seed clone: %v", err)
	}
}

func fieldDiffByLocator(t *testing.T, artifact *DiffArtifact, locator string) *FieldDiff {
	t.Helper()
	for i := range artifact.Fields {
		if artifact.Fields[i].Locator == locator {
			return &artifact.Fields[i]
		}
	}
	t.Fatalf("no diff entry for %s: %+v", locator, artifact.Fields)
	return nil
}

func defaultRunner(t *testing.T, field, value string) *fakeImproveRunner {
	t.Helper()
	return &fakeImproveRunner{verdicts: map[string]func() string{
		field: improveVerdictFor(t, map[string]any{"value_string": value}),
	}}
}

func testLoggerT() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
