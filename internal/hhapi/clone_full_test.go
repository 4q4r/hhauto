package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// T27 full-clone fixtures: synthetic builder-state payloads (no real PII —
// titles are "DevOps-инженер Test", experience is "Тест-компания").

// builderStateFixture renders a /profile/shards/resume/get_by_hash body.
func builderStateFixture(hash, status, title string, complete bool) string {
	titleField := "[]"
	if title != "" {
		titleField = `[{"string":` + mustJSON(title) + `}]`
	}
	next := "null"
	if !complete {
		next = `"professional_role"`
	}
	return `{
	  "nextIncompleteScreenId": ` + next + `,
	  "screens": [
	    {"screenType":"STATIC","screenId":"professional_role","title":"Роль","fieldsOptions":{},"screenContent":[],"messages":[]},
	    {"screenType":"STATIC","screenId":"common","title":"Основное","fieldsOptions":{},"screenContent":[],"messages":[]}
	  ],
	  "resume": {
	    "_attributes": {"hash":` + mustJSON(hash) + `,"status":` + mustJSON(status) + `,"isSearchable":` + map[bool]string{true: "true", false: "false"}[status == "modified"] + `,"hasErrors":` + map[bool]string{true: "false", false: "true"}[title != ""] + `},
	    "title": ` + titleField + `,
	    "professionalRole": [{"string":160}],
	    "keySkills": [{"string":"Linux"},{"string":"Docker"}],
	    "skills": [{"string":"Автоматизация инфраструктуры (тестовые данные)"}],
	    "workFormats": [{"string":"REMOTE"}],
	    "educationLevel": []
	  },
	  "profile": {
	    "fields": {
	      "area": [{"string":1550}],
	      "citizenship": [{"string":113}],
	      "experience": [{"id":1,"startDate":"2024-01-01","endDate":null,"companyName":"Тест-компания","position":"DevOps-инженер Test","description":"Тестовое описание"}]
	    }
	  },
	  "additionalProperties": {"anyJob":false,"jobByEducation":null,"simpleExperienceInYears":false}
	}`
}

func mustJSON(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// serveBuilderSurface wires the builder get_by_hash + update endpoints used
// by the T27 CloneResume flow. parentHash is served the published parent
// fixture; every other hash is served the mutable clone state
// (env.cloneBuilderState). updateOK=false makes the update endpoint reject
// with a bare 400 (the live 2026-09-25 behavior); a successful update flips
// the clone state to complete so the verify gate can read it back. The
// shell-clone endpoint stays test-specific.
func (env *testEnv) serveBuilderSurface(parentHash string, updateOK bool) {
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Query().Get("resumeHash") == parentHash {
			_, _ = w.Write([]byte(builderStateFixture(parentHash, "modified", "DevOps-инженер Test", true)))
			return
		}
		// The clone state the verify gate reads: complete only after a
		// successful materialization flips it below.
		_, _ = w.Write([]byte(env.cloneBuilderState))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/update", func(w http.ResponseWriter, _ *http.Request) {
		// env.serve already consumed and captured the body; assert on the
		// captured copy.
		body := env.lastBody("1:POST /profile/shards/resume/update")
		if !strings.Contains(body, `"resumeHash":"`) || strings.Contains(body, `"resumeHash":"p4rent"`) || strings.Contains(body, `"resumeHash":"h4sh1"`) {
			http.Error(w, "wrong clone", http.StatusBadRequest)
			return
		}
		if !updateOK {
			// Live hh behavior observed 2026-09-25: 400 with an empty body.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		env.cloneBuilderState = builderStateFixture("cl0neh4sh", "modified", "DevOps-инженер Test", true)
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null,"resume":{"_attributes":{"hash":"fl1pped","status":"modified"}}}`))
	})
}

// serveFullCloneFlow wires the standard shell-clone envelope plus the full
// builder surface (parent hash "p4rent").
func (env *testEnv) serveFullCloneFlow(updateOK bool) {
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"/profile/resume/?resume=cl0neh4sh"}`))
	})
	env.serveBuilderSurface("p4rent", updateOK)
}

// T27: the full flow — shell clone, materialization through the builder
// update endpoint, verify read-back — returns the new hash only after the
// clone reads back complete (title present, status left not_finished).
func TestCloneResumeFullFlow(t *testing.T) {
	env := newTestEnv(t)
	env.cloneBuilderState = builderStateFixture("cl0neh4sh", "not_finished", "", false)
	env.serveFullCloneFlow(true)
	c := env.client()
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "cl0neh4sh" {
		t.Fatalf("hash = %q, want cl0neh4sh", hash)
	}
	if env.hits("1:POST /profile/shards/resume/update") == 0 {
		t.Fatal("no update calls: materialization never ran")
	}
	if env.hits("1:GET /profile/shards/resume/get_by_hash") < 2 {
		t.Fatalf("get_by_hash hits = %d, want >= 2 (parent + verify)", env.hits("1:GET /profile/shards/resume/get_by_hash"))
	}
	// The update body carries the builder contract: hash, screen, split sides.
	var upd struct {
		ResumeHash      string         `json:"resumeHash"`
		CurrentScreenID string         `json:"currentScreenId"`
		Resume          map[string]any `json:"resume"`
		Profile         map[string]any `json:"profile"`
		Additional      map[string]any `json:"additionalProperties"`
	}
	if err := json.Unmarshal([]byte(env.lastBody("1:POST /profile/shards/resume/update")), &upd); err != nil {
		t.Fatalf("update body: %v", err)
	}
	if upd.ResumeHash != "cl0neh4sh" {
		t.Fatalf("update resumeHash = %q", upd.ResumeHash)
	}
	if upd.CurrentScreenID == "" {
		t.Fatal("update carries no currentScreenId")
	}
	if _, ok := upd.Resume["title"]; !ok {
		t.Fatal("update resume side misses the parent title (content transfer)")
	}
	if _, ok := upd.Profile["experience"]; !ok {
		t.Fatal("update profile side misses experience (hM split)")
	}
	if _, ok := upd.Additional["anyJob"]; !ok {
		t.Fatal("update carries no additionalProperties (anyJob)")
	}
}

// T27: the update endpoint rejecting (the live 2026-09-25 behavior) must be a
// typed materialize failure — never a returned hash.
func TestCloneResumeMaterializeRejects(t *testing.T) {
	env := newTestEnv(t)
	env.cloneBuilderState = builderStateFixture("cl0neh4sh", "not_finished", "", false)
	env.serveFullCloneFlow(false)
	c := env.client()
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if hash != "" {
		t.Fatalf("hash = %q on materialize failure, want empty", hash)
	}
	var ce *CloneError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CloneError", err)
	}
	if ce.Stage != StageMaterialize {
		t.Fatalf("stage = %q, want %q", ce.Stage, StageMaterialize)
	}
	if ce.Hash != "cl0neh4sh" {
		t.Fatalf("clone hash = %q, want cl0neh4sh", ce.Hash)
	}
	if !errors.Is(err, ErrCloneMaterialize) {
		t.Fatalf("err = %v, want ErrCloneMaterialize wrap", err)
	}
}

// T27: hh claiming success (nextIncomplete null) while the clone still reads
// back not_finished with no title must fail the verify gate — the shell is
// never returned as a usable clone.
func TestCloneResumeVerifyGate(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"/profile/resume/?resume=cl0neh4sh"}`))
	})
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Query().Get("resumeHash") == "cl0neh4sh" {
			_, _ = w.Write([]byte(builderStateFixture("cl0neh4sh", "not_finished", "", false)))
			return
		}
		_, _ = w.Write([]byte(builderStateFixture("p4rent", "modified", "DevOps-инженер Test", true)))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/update", func(w http.ResponseWriter, _ *http.Request) {
		// hh accepts the update (null next) but the state does not flip —
		// the verify gate must catch the divergence.
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if hash != "" {
		t.Fatalf("hash = %q on verify failure, want empty", hash)
	}
	var ce *CloneError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CloneError", err)
	}
	if ce.Stage != StageVerify {
		t.Fatalf("stage = %q, want %q", ce.Stage, StageVerify)
	}
	if !errors.Is(err, ErrCloneIncomplete) {
		t.Fatalf("err = %v, want ErrCloneIncomplete wrap", err)
	}
}

// T27: the verify gate runs on the builder state — parse helpers it relies on
// must read the condition-format title and the _attributes status.
func TestBuilderStateParsing(t *testing.T) {
	st, err := parseBuilderState([]byte(builderStateFixture("h4sh", "modified", "DevOps-инженер Test", true)))
	if err != nil {
		t.Fatalf("parseBuilderState: %v", err)
	}
	if got := st.Title(); got != "DevOps-инженер Test" {
		t.Fatalf("title = %q", got)
	}
	if st.Status() != "modified" {
		t.Fatalf("status = %q", st.Status())
	}
	if st.Hash != "h4sh" {
		t.Fatalf("hash = %q", st.Hash)
	}
	if len(st.Screens) != 2 || st.Screens[0] != "professional_role" {
		t.Fatalf("screens = %v", st.Screens)
	}
	if st.NextIncomplete != nil {
		t.Fatalf("nextIncomplete = %v, want nil", *st.NextIncomplete)
	}
	// The shell: empty title, not_finished — exactly what hh returns for a
	// freshly cloned shell (live 2026-09-25).
	shell, err := parseBuilderState([]byte(builderStateFixture("sh3ll", "not_finished", "", false)))
	if err != nil {
		t.Fatalf("parseBuilderState(shell): %v", err)
	}
	if shell.Title() != "" || shell.Status() != "not_finished" {
		t.Fatalf("shell title=%q status=%q", shell.Title(), shell.Status())
	}
}

// T27: the hM split — personal fields (+experience) ride the profile side,
// resume content (title, roles, skills, conditions) stays on the resume side.
func TestSplitBuilderResume(t *testing.T) {
	st, err := parseBuilderState([]byte(builderStateFixture("h4sh", "modified", "DevOps-инженер Test", true)))
	if err != nil {
		t.Fatalf("parseBuilderState: %v", err)
	}
	resumeSide, profileFields := splitBuilderResume(st)
	for _, k := range []string{"title", "professionalRole", "keySkills", "skills", "workFormats"} {
		if _, ok := resumeSide[k]; !ok {
			t.Fatalf("resume side misses %q", k)
		}
	}
	// Identity metadata never travels with the content.
	for _, k := range []string{"_attributes", "_conditions"} {
		if _, ok := resumeSide[k]; ok {
			t.Fatalf("identity field %q must not travel to the resume side", k)
		}
	}
	for _, k := range []string{"area", "citizenship", "experience"} {
		if _, ok := profileFields[k]; !ok {
			t.Fatalf("profile side misses %q", k)
		}
	}
	if _, ok := resumeSide["area"]; ok {
		t.Fatal("area leaked to the resume side (hM split violation)")
	}
	if _, ok := resumeSide["experience"]; ok {
		t.Fatal("experience leaked to the resume side (hM split violation)")
	}
}
