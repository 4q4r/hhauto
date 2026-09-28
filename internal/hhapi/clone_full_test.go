package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Builder-state fixtures shared by the clone and content-editor tests:
// synthetic payloads (no real PII — titles are "DevOps-инженер Test",
// experience is "Тест-компания").

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

// T45: the native verify gate — clone_resumes answered {} and the shard
// discovery found a parented clone, but the clone reads back as an untitled
// not_finished shell. That divergence must fail typed (CloneError/verify +
// ErrCloneIncomplete): the hash is never returned for an unpublished shell.
func TestCloneResumeNativeVerifyGate(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Query().Get("resumeHash") == "p4rent" {
			_, _ = w.Write([]byte(cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", nil)))
			return
		}
		// hh claiming a parented entry while the state itself is still an
		// empty draft — exactly the divergence the gate exists for.
		_, _ = w.Write([]byte(builderStateFixture("sh3llcl0ne", "not_finished", "", false)))
	})
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(cloneShardFixture("285632195", "sh3llcl0ne", 0)))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	c := nativeEndpoints(env)
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
	if ce.Hash != "sh3llcl0ne" {
		t.Fatalf("clone hash = %q, want sh3llcl0ne", ce.Hash)
	}
	if !errors.Is(err, ErrCloneIncomplete) {
		t.Fatalf("err = %v, want ErrCloneIncomplete wrap", err)
	}
}

// The verify gate parses the builder state — condition-format title and
// _attributes status/isSearchable are the fields it reads.
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
	if !st.IsSearchable {
		t.Fatal("isSearchable = false, want true")
	}
	if len(st.Screens) != 2 || st.Screens[0] != "professional_role" {
		t.Fatalf("screens = %v", st.Screens)
	}
	if st.NextIncomplete != nil {
		t.Fatalf("nextIncomplete = %v, want nil", *st.NextIncomplete)
	}
	// The shell: empty title, not_finished — what an unpublished draft reads
	// back as (live 2026-09-25, T46 verdict).
	shell, err := parseBuilderState([]byte(builderStateFixture("sh3ll", "not_finished", "", false)))
	if err != nil {
		t.Fatalf("parseBuilderState(shell): %v", err)
	}
	if shell.Title() != "" || shell.Status() != "not_finished" || shell.IsSearchable {
		t.Fatalf("shell title=%q status=%q searchable=%t", shell.Title(), shell.Status(), shell.IsSearchable)
	}
}

// The hM split — personal fields (+experience) ride the profile side,
// resume content (title, roles, skills, conditions) stays on the resume
// side. Consumed by the T29 content editor, not by cloning (T45).
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

// Guard the removed shell surface: no CODE in the package sources may
// reference the endpoint that only ever created not_finished drafts (T46
// live verdict). Doc comments documenting the removal are fine — line
// comments are stripped before the scan.
func TestShellCloneEndpointAbsent(t *testing.T) {
	for _, f := range []string{"clone.go", "endpoints.go", "contracts.go", "client.go", "parsers.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			if strings.Contains(line, "applicant/resumes/clone") {
				t.Fatalf("%s:%d references the removed shell clone endpoint in code", f, i+1)
			}
		}
	}
}
