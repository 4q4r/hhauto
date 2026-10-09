package cli

// T96 E2E: the create→improve chain (owner decision, variant «а») through
// the real cobra tree with a fake hh surface and a fake OpenAI-compatible
// AI endpoint. A successful `resume create` automatically runs the improve
// pipeline over the fresh shell in the same process; --no-improve keeps
// the bare shell and must never reach the editor.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// createHHState is the mutable hh surface the chain fake serves: the
// resumes shard (grows by one not_finished shell after the create POST)
// and the builder state the improve pipeline reads and edits.
type createHHState struct {
	mu      sync.Mutex
	created bool
	Title   string
	edits   int
	creates []string
}

const t96ShellHash = "newsh1ff112c34820039ed1f6000111222334"

func (s *createHHState) shardBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := []string{`{"hash":"res-h1","title":"Go разработчик",` +
		`"updatedAt":"2026-09-24T10:00:00Z",` +
		`"status":{"id":"published","name":"Опубликовано"},` +
		`"_attributes":{"isSearchable":true,"percent":95,"status":{"id":"published","name":"Опубликовано"}}}`}
	if s.created {
		entries = append(entries, `{"hash":"`+t96ShellHash+`","title":"","updatedAt":"2026-10-09T10:00:00Z",`+
			`"status":{"id":"not_finished","name":"Черновик"},`+
			`"_attributes":{"isSearchable":false,"percent":0,"status":{"id":"not_finished","name":"Черновик"}}}`)
	}
	return `{"resumes":[` + strings.Join(entries, ",") + `]}`
}

func (s *createHHState) builderStateBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return `{
	  "nextIncompleteScreenId": "professional_role",
	  "screens": [{"screenType":"STATIC","screenId":"title","title":"Должность","fieldsOptions":{},"screenContent":[],"messages":[]}],
	  "resume": {
	    "_attributes": {"hash":"` + t96ShellHash + `","status":"not_finished","isSearchable":false},
	    "title": [{"string":` + mustJSONT(s.Title) + `}]
	  },
	  "profile": {"fields": {}},
	  "additionalProperties": {}
	}`
}

func (s *createHHState) editCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.edits
}

// newFakeHHCreate serves the hh surface the chain needs: bootstrap,
// chats, search, the resumes shard, the create POST, the builder
// get_by_hash and the T51 resume/edit route.
func newFakeHHCreate(t *testing.T, st *createHHState) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(settingsPage))
	})
	mux.HandleFunc("/chatik/api/chats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chats":{"items":[]},"chatsDisplayInfo":{}}`))
	})
	mux.HandleFunc("/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(st.shardBody()))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(emptySearchPage))
	})
	mux.HandleFunc("/profile/shards/resume/create", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&payload)
		raw, _ := json.Marshal(payload)
		st.mu.Lock()
		st.creates = append(st.creates, string(raw))
		st.created = true
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":"professional_role","screens":[]}`))
	})
	mux.HandleFunc("/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(st.builderStateBody()))
	})
	mux.HandleFunc("/applicant/resume/edit", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&payload)
		var title []string
		if raw, ok := payload["title"]; ok {
			_ = json.Unmarshal(raw, &title)
		}
		st.mu.Lock()
		st.edits++
		if len(title) > 0 {
			st.Title = title[0]
		}
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(st.builderStateBody()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// t96CreateExtra wires the create command to the fake hh and the fake AI:
// improve on the title field, the BFF-hosted endpoints pinned to the fake.
func t96CreateExtra(aiURL string) string {
	return `
[features.autoclone.improve]
enabled = false
fields = ["title"]
model_profile = "main-model"

[ai.providers.fast]
dialect = "openai-chat"
base_url = "` + aiURL + `"
api_key_env = "T96_AI_KEY"

[ai.models.main-model]
provider = "fast"
model = "test-model"

[api.endpoints.override]
resume_edit = "` + testSite + `/applicant/resume/edit"
resume_create = "` + testSite + `/profile/shards/resume/create"
`
}

// The chain (variant «а»): create succeeds → the improve pipeline fills
// the fresh shell in the same process (the AI title lands through the
// editor route). The shell is empty everywhere, so the fill grounds on
// profile.md (FillEmptyFromProfile, T96).
func TestResumeCreateImproveChainE2E(t *testing.T) {
	st := &createHHState{}
	testSite = newFakeHHCreate(t, st)
	aiURL := newFakeAI(t, t35Verdict)
	t.Setenv("T96_AI_KEY", "test-key")
	cfgPath := writeTestConfig(t, t96CreateExtra(aiURL))
	profileMD := filepath.Join(filepath.Dir(cfgPath), "profile.md")
	if werr := os.WriteFile(profileMD, []byte("# Профиль\n\n- Целевые роли: DevOps-инженер, Backend-разработчик\n- Стек: Linux, Docker, Go, Python\n- Опыт: коммерческая разработка и автоматизация инфраструктуры\n"), 0o600); werr != nil {
		t.Fatalf("write profile.md: %v", werr)
	}

	out, err := runRoot(t, "resume", "create", "--title", "Тест T96", "--config", cfgPath)
	if err != nil {
		t.Fatalf("create chain: %v\nedits=%d title=%q\n%s", err, st.editCount(), st.title(), out)
	}
	mustContain(t, out, t96ShellHash, "Резюме создано", "применено")
	if st.editCount() == 0 {
		t.Fatalf("the improve pipeline never reached the editor: edits=0 title=%q\n%s", st.title(), out)
	}
	if got := st.title(); got != "DevOps-инженер (автоматизация)" {
		t.Fatalf("shell title after the chain = %q, want the AI verdict value", got)
	}
}

// --no-improve: the bare shell only — the editor route is never touched.
func TestResumeCreateNoImproveE2E(t *testing.T) {
	st := &createHHState{}
	testSite = newFakeHHCreate(t, st)
	aiURL := newFakeAI(t, t35Verdict)
	t.Setenv("T96_AI_KEY", "test-key")
	cfgPath := writeTestConfig(t, t96CreateExtra(aiURL))

	out, err := runRoot(t, "resume", "create", "--title", "Тест T96", "--no-improve", "--config", cfgPath)
	if err != nil {
		t.Fatalf("create --no-improve: %v\n%s", err, out)
	}
	mustContain(t, out, t96ShellHash, "Резюме создано")
	if strings.Contains(out, "improve diff") {
		t.Fatalf("--no-improve must not run the improve pipeline:\n%s", out)
	}
	if edits := st.editCount(); edits != 0 {
		t.Fatalf("--no-improve reached the editor %d times", edits)
	}
}

func (s *createHHState) title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}
