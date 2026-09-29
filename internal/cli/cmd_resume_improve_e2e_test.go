package cli

// T35 E2E: the `resume improve` command through the real cobra tree with
// a fake hh builder surface and a fake OpenAI-compatible AI endpoint.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hhauto/internal/store"
)

// improveHHState is the mutable builder state the fake hh serves.
type improveHHState struct {
	mu    sync.Mutex
	Title string
}

func (s *improveHHState) builderStateBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return `{
	  "nextIncompleteScreenId": null,
	  "screens": [{"screenType":"STATIC","screenId":"title","title":"Должность","fieldsOptions":{},"screenContent":[],"messages":[]}],
	  "resume": {
	    "_attributes": {"hash":"b4se1","status":"modified","isSearchable":true},
	    "title": [{"string":` + mustJSONT(s.Title) + `}],
	    "keySkills": [{"string":"Linux"}],
	    "skills": [{"string":` + mustJSONT(s.Title) + `}]
	  },
	  "profile": {"fields": {}},
	  "additionalProperties": {}
	}`
}

func mustJSONT(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// newFakeHHImprove serves the base hh surface plus the builder
// get_by_hash/update shards and (optionally) a failing settings endpoint.
func newFakeHHImprove(t *testing.T, settingsLostSession bool, st *improveHHState) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		if settingsLostSession {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":[{"value":"forbidden"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(settingsPage))
	})
	mux.HandleFunc("/chatik/api/chats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chats":{"items":[]},"chatsDisplayInfo":{}}`))
	})
	mux.HandleFunc("/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if settingsLostSession {
			// The bootstrap shard is the session probe: 401 maps to
			// hhapi.ErrLoggedOut (the session-loss contract).
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"value":"not_authorized"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"resumes":[{"hash":"res-h1","title":"Go разработчик",` +
			`"updatedAt":"2026-09-24T10:00:00Z",` +
			`"status":{"id":"published","name":"Опубликовано"},` +
			`"_attributes":{"isSearchable":true,"percent":95,"status":{"id":"published","name":"Опубликовано"}}}]}`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(emptySearchPage))
	})
	mux.HandleFunc("/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(st.builderStateBody()))
	})
	mux.HandleFunc("/profile/shards/resume/update", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Resume map[string]json.RawMessage `json:"resume"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		var title []struct {
			String string `json:"string"`
		}
		if raw, ok := payload.Resume["title"]; ok {
			_ = json.Unmarshal(raw, &title)
		}
		if len(title) > 0 {
			st.mu.Lock()
			st.Title = title[0].String
			st.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newFakeAIServe serves one OpenAI-chat completions endpoint returning the
// scripted verdict content.
func newFakeAI(t *testing.T, verdict string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		content, _ := json.Marshal(verdict)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const t35Verdict = `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,` +
	`"injection_suspected":false,"unchanged":false,"used_facts":[],"missing_data":[],` +
	`"value_string":"DevOps-инженер (автоматизация)","value_strings":null,"value_amount":null}`

// t35ImproveExtra is the config extra wiring the improve command to the
// fake AI and pinning the builder-update endpoint onto the fake hh site
// (production defaults to the absolute resume-profile-front URL).
func t35ImproveExtra(aiURL string) string {
	return `
[features.autoclone.improve]
enabled = false
fields = ["title"]
model_profile = "main-model"

[ai.providers.fast]
dialect = "openai-chat"
base_url = "` + aiURL + `"
api_key_env = "T35_AI_KEY"

[ai.models.main-model]
provider = "fast"
model = "test-model"

[api.endpoints.override]
resume_update = "` + testSite + `/profile/shards/resume/update"
`
}

// seedBaseResumeSnapshot opens the config's store and records a BASE
// resume snapshot (no clone lineage).
func seedBaseResumeSnapshot(t *testing.T, dataDir string) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(dataDir, "hhauto.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatalf("seed store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.AccountsUpsert(ctx, store.Account{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ResumesUpsert(ctx, &store.Resume{AccountID: "main", HhResumeID: "b4se1", Title: "Базовое резюме"}); err != nil {
		t.Fatal(err)
	}
}

// The base-resume invariant: without --confirm the command refuses; with
// it the pipeline is allowed through (the missing AI config is the NEXT
// failure, proving the gate was passed).
func TestResumeImproveConfirmGateE2E(t *testing.T) {
	st := &improveHHState{Title: "Базовое резюме"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, "")
	seedBaseResumeSnapshot(t, filepath.Dir(cfgPath))

	_, err := runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("base resume without --confirm: err = %v", err)
	}

	_, err = runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath, "--confirm")
	if err == nil || !strings.Contains(err.Error(), "AI-раннер") {
		t.Fatalf("gate must pass and fail on the missing AI config: err = %v", err)
	}
}

// The happy path: dry-run computes the diff and mutates nothing; the real
// run applies through the builder-update API and prints applied artifact.
func TestResumeImproveHappyPathE2E(t *testing.T) {
	st := &improveHHState{Title: "Базовое резюме"}
	testSite = newFakeHHImprove(t, false, st)
	aiURL := newFakeAI(t, t35Verdict)
	extra := t35ImproveExtra(aiURL)
	t.Setenv("T35_AI_KEY", "test-key")
	cfgPath := writeTestConfig(t, extra)
	dataDir := filepath.Dir(cfgPath)
	seedBaseResumeSnapshot(t, dataDir)

	out, err := runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath, "--confirm", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run improve: %v\n%s", err, out)
	}
	mustContain(t, out, "dry-run", "improved")
	// T52: the start event is logged once (console handler); a manual
	// stdout duplicate must not come back.
	if strings.Contains(out, "resume improve: hash=") {
		t.Fatalf("duplicated start banner in output:\n%s", out)
	}
	if st.title() != "Базовое резюме" {
		t.Fatalf("dry run mutated the resume: %q", st.title())
	}

	out, err = runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath, "--confirm")
	if err != nil {
		t.Fatalf("improve: %v\n%s", err, out)
	}
	mustContain(t, out, "применено")
	if got := st.title(); got != "DevOps-инженер (автоматизация)" {
		t.Fatalf("resume title after improve = %q", got)
	}
}

func (s *improveHHState) title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}

// The unknown --fields value fails fast before any AI call.
func TestResumeImproveUnknownFieldE2E(t *testing.T) {
	st := &improveHHState{Title: "Базовое резюме"}
	testSite = newFakeHHImprove(t, false, st)
	aiURL := newFakeAI(t, t35Verdict)
	extra := t35ImproveExtra(aiURL)
	t.Setenv("T35_AI_KEY", "test-key")
	cfgPath := writeTestConfig(t, extra)
	seedBaseResumeSnapshot(t, filepath.Dir(cfgPath))

	_, err := runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath,
		"--confirm", "--fields", "title,bogus_field")
	if err == nil || !strings.Contains(err.Error(), "bogus_field") {
		t.Fatalf("unknown field must fail fast: %v", err)
	}
}

// The resolve fallback: a hash tracked as a CLONE improves without
// --confirm (lineage-owned resume).
func TestResumeImproveCloneNoConfirmNeededE2E(t *testing.T) {
	st := &improveHHState{Title: "Клон"}
	testSite = newFakeHHImprove(t, false, st)
	aiURL := newFakeAI(t, t35Verdict)
	extra := t35ImproveExtra(aiURL)
	t.Setenv("T35_AI_KEY", "test-key")
	cfgPath := writeTestConfig(t, extra)
	db, err := store.Open(context.Background(), filepath.Join(filepath.Dir(cfgPath), "hhauto.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.AccountsUpsert(ctx, store.Account{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ResumeClonesRegister(ctx, &store.ResumeClone{
		CloneHash: "b4se1", ParentHash: "p4rent", Depth: 1, AccountID: "main",
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	out, err := runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath, "--dry-run")
	if err != nil {
		t.Fatalf("clone improve without confirm: %v\n%s", err, out)
	}
	mustContain(t, out, "dry-run")
	// The diff persisted on the clone's lineage row.
	db2, err := store.Open(ctx, filepath.Join(filepath.Dir(cfgPath), "hhauto.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	row, err := db2.ResumeClonesGet(ctx, "b4se1")
	if err != nil || row == nil {
		t.Fatalf("lineage row: %v %v", row, err)
	}
	if !strings.Contains(row.ImproveDiff, "resume.title") {
		t.Fatalf("stored diff = %q", row.ImproveDiff)
	}
}
