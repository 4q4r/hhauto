package hhapi

// T51 resume content mutation tests: the save payload is the REAL editor
// UI contract (captured live 2026-09-29 from the resume_partial_edit
// flow, clone 4f48f357…):
//
//	POST <site>/applicant/resume/edit?resume={hash}&hhtmSource=resume_partial_edit
//	{"title":["…"]} — one flat field array per POST (title-only accepted
//	live, 200); success answers the builder-state envelope, rejections a
//	200 {"errors":[{field,code,…}]}.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// contentFixture renders a complete builder state for the improve flow:
// a published resume (nextIncompleteScreenId null) whose split content the
// feature mutates field by field.
func contentFixture(hash, title, about string) string {
	return `{
	  "nextIncompleteScreenId": null,
	  "screens": [
	    {"screenType":"STATIC","screenId":"title","title":"Должность"},
	    {"screenType":"STATIC","screenId":"key_skills","title":"Ключевые навыки"},
	    {"screenType":"STATIC","screenId":"about","title":"О себе"}
	  ],
	  "resume": {
	    "_attributes": {"hash":` + mustJSON(hash) + `,"id":"285632195","status":"new","isSearchable":true},
	    "title": [{"string":` + mustJSON(title) + `}],
	    "skills": [{"string":` + mustJSON(about) + `}],
	    "keySkills": [{"string":"Linux"}],
	    "salary": [{"amount": 0, "currency": "RUR"}],
	    "_conditions": {}
	  },
	  "profile": {"fields": {"experience": [], "citizenship": [{"string":"1550"}]}},
	  "additionalProperties": {}
	}`
}

// contentSurface is a stateful fake of the builder surface: GET returns the
// current fixture, POST update applies the requested resume-side changes
// the way hh would (when apply=true) and records every body/header.
type contentSurface struct {
	mu     sync.Mutex
	state  string
	apply  bool
	bodies []string
}

func (s *contentSurface) mount(env *testEnv) {
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.state))
	})
	env.serve(env.mux1, "1", "POST", "/applicant/resume/edit", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body := env.lastBody("1:POST /applicant/resume/edit")
		s.bodies = append(s.bodies, body)
		// The captured UI header set plus the partial-edit addressing.
		if r.Header.Get("X-hhtmSource") != "resume_partial_edit" || r.Header.Get("X-Requested-With") == "" ||
			r.Header.Get("X-Xsrftoken") == "" || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.URL.Query().Get("hhtmSource") != "resume_partial_edit" || r.URL.Query().Get("resume") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s.apply {
			s.state = applyEditField(s.state, body)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.state))
	})
}

// applyEditField rewrites the served fixture's field when the edit body
// carries it — the fake hh apply. The wire value is FLAT (["x"]); the
// builder state stores conditional format ([{"string":"x"}]), like live hh
// (flat in, conditional read-back, live 2026-09-29).
func applyEditField(state, body string) string {
	var p map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &p); err != nil || len(p) == 0 {
		return state
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(state), &root); err != nil {
		return state
	}
	var resume map[string]json.RawMessage
	if err := json.Unmarshal(root["resume"], &resume); err != nil {
		return state
	}
	for name, flat := range p {
		var items []json.RawMessage
		if err := json.Unmarshal(flat, &items); err != nil {
			continue
		}
		cond := make([]map[string]json.RawMessage, 0, len(items))
		for _, item := range items {
			cond = append(cond, map[string]json.RawMessage{"string": item})
		}
		wrapped, err := json.Marshal(cond)
		if err != nil {
			continue
		}
		resume[name] = wrapped
	}
	root["resume"], _ = json.Marshal(resume)
	out, _ := json.Marshal(root)
	return string(out)
}

func (s *contentSurface) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *contentSurface) lastUpdate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return ""
	}
	return s.bodies[len(s.bodies)-1]
}

func contentEndpoints(env *testEnv) (c *Client) {
	env.t.Helper()
	eps := env.endpoints()
	if eps.Override == nil {
		eps.Override = map[string]string{}
	}
	eps.Override["resume_edit"] = env.srv1.URL + "/applicant/resume/edit"
	c, err := NewClient(env.account(), eps, nil, nil, WithBrowserTLSSpec(captureSpec(env.t)))
	if err != nil {
		env.t.Fatalf("NewClient: %v", err)
	}
	c.setXSRFForTests("tok_test")
	return c
}

// mutatedContent applies the feature-shaped change to the split: new title
// on the resume side.
func mutatedContent(base *ResumeContent, newTitle string) *ResumeContent {
	out := &ResumeContent{
		Resume:  map[string]json.RawMessage{},
		Profile: map[string]json.RawMessage{},
		Screens: base.Screens,
	}
	for k, v := range base.Resume {
		out.Resume[k] = v
	}
	for k, v := range base.Profile {
		out.Profile[k] = v
	}
	out.Resume["title"], _ = json.Marshal([]map[string]string{{"string": newTitle}})
	return out
}

// T29: the builder-state conditions must survive the split — the feature
// validators read the live hh limits (minlength/maxcount/…) from here.
func TestResumeContentCarriesConditions(t *testing.T) {
	env := newTestEnv(t)
	fixture := strings.Replace(contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию"),
		`"_conditions": {}`,
		`"_conditions": {"title": {"parts": [{"string": {"minlength": 2, "maxlength": 100}}]}}`, 1)
	surface := &contentSurface{state: fixture}
	surface.mount(env)
	c := contentEndpoints(env)

	got, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	var titleCond struct {
		Parts []struct {
			String struct {
				MinLength int `json:"minlength"`
				MaxLength int `json:"maxlength"`
			} `json:"string"`
		} `json:"parts"`
	}
	raw, ok := got.Conditions["title"]
	if !ok {
		t.Fatalf("conditions missing title: %v", got.Conditions)
	}
	if err := json.Unmarshal(raw, &titleCond); err != nil {
		t.Fatalf("decode conditions: %v", err)
	}
	if len(titleCond.Parts) == 0 || titleCond.Parts[0].String.MaxLength != 100 {
		t.Fatalf("title condition = %s", raw)
	}
}

// T29: ResumeContent returns the mutable split (content on resume side,
// personal + experience on profile side) plus the editor screens.
func TestResumeContentRead(t *testing.T) {
	env := newTestEnv(t)
	surface := &contentSurface{state: contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию")}
	surface.mount(env)
	c := contentEndpoints(env)

	got, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	if s := conditionStrings(got.Resume["title"]); len(s) != 1 || s[0] != "DevOps-инженер Test" {
		t.Fatalf("title = %v", s)
	}
	if _, ok := got.Resume["_attributes"]; ok {
		t.Fatal("_attributes must never travel in the mutable content")
	}
	if _, ok := got.Profile["experience"]; !ok {
		t.Fatal("experience must ride the profile side")
	}
	if _, ok := got.Profile["citizenship"]; !ok {
		t.Fatal("profile.fields must merge into the profile side")
	}
	if len(got.Screens) != 3 || got.Screens[0] != "title" {
		t.Fatalf("screens = %v", got.Screens)
	}
}

// The hh shared records (experience / education items are shared across
// the account's resumes): the split keeps only the items whose "resumes"
// usage list carries this resume's hash — the inherited parent-resume
// records are not this resume's content and posting them makes hh fork
// copies — and strips the server-only "resumes"/"companyLogos" fields the
// UI save body never carries.
func TestResumeContentFiltersSharedRecords(t *testing.T) {
	env := newTestEnv(t)
	fixture := strings.Replace(contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию"),
		`"experience": []`,
		`"experience": [
		   {"id":1,"companyName":"Своя","position":"Dev","description":"д","companyLogos":[{"small":"s"}],"resumes":[{"hash":"cl0ne1"}]},
		   {"id":2,"companyName":"Родительская","position":"Old","description":"др","resumes":[{"hash":"p4rent"},{"hash":"cl0ne1x"}]}
		 ],
		 "primaryEducation": [{"id":3,"name":"ВУЗ","year":2028,"resumes":[{"hash":"p4rent"}]}]`, 1)
	surface := &contentSurface{state: fixture}
	surface.mount(env)
	c := contentEndpoints(env)

	got, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	var exp []map[string]json.RawMessage
	if err := json.Unmarshal(got.Profile["experience"], &exp); err != nil {
		t.Fatalf("decode experience: %v", err)
	}
	if len(exp) != 1 {
		t.Fatalf("experience items = %d, want 1 (only the resume's own record)", len(exp))
	}
	if string(exp[0]["companyName"]) != `"Своя"` {
		t.Fatalf("kept the wrong record: %s", exp[0]["companyName"])
	}
	for _, meta := range []string{"resumes", "companyLogos"} {
		if _, has := exp[0][meta]; has {
			t.Fatalf("experience item still carries %s", meta)
		}
	}
	var edu []map[string]json.RawMessage
	if err := json.Unmarshal(got.Profile["primaryEducation"], &edu); err != nil {
		t.Fatalf("decode primaryEducation: %v", err)
	}
	if len(edu) != 0 {
		t.Fatalf("inherited primaryEducation must not ride the split, got %d items", len(edu))
	}
	if s := conditionStrings(got.Resume["title"]); len(s) != 1 || s[0] != "DevOps-инженер Test" {
		t.Fatalf("title must survive the filter: %v", s)
	}
}

// T51: the edit POST carries the REAL UI contract — one flat field array
// on the partial-edit URL with the captured header set — and the change is
// verified by read-back.
func TestUpdateResumeContentPostsEditContract(t *testing.T) {
	env := newTestEnv(t)
	surface := &contentSurface{state: contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию"), apply: true}
	surface.mount(env)
	c := contentEndpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	mutated := mutatedContent(base, "DevOps-инженер Test (копия)")
	if err := c.UpdateResumeContent(context.Background(), "cl0ne1", mutated, []string{"title"}); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}
	if got := surface.updateCount(); got != 1 {
		t.Fatalf("one changed field must ride exactly one edit POST, got %d", got)
	}
	body := surface.lastUpdate()
	// The golden wire body: the conditional-format title flattens to a
	// plain string array — exactly what the live UI POSTs.
	if body != `{"title":["DevOps-инженер Test (копия)"]}` {
		t.Fatalf("edit body = %s", body)
	}
	if strings.Contains(body, "_attributes") || strings.Contains(body, "_conditions") {
		t.Fatal("identity metadata must not travel with the content")
	}
}

// editFlatValue covers every live value shape (2026-09-29 captures):
// conditional strings flatten, conditional numbers flatten with their JSON
// type, empty arrays and experience objects travel verbatim.
func TestEditFlatValueShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"conditional string", `[{"string":"Копия T51"}]`, `["Копия T51"]`},
		{"conditional number", `[{"string":40}]`, `[40]`},
		{"string list", `[{"string":"Linux"},{"string":"Docker"}]`, `["Linux","Docker"]`},
		{"empty stays", `[]`, `[]`},
		{"experience verbatim", `[{"id":2103588721,"startDate":"2024-01-01","description":"…"}]`, `[{"id":2103588721,"startDate":"2024-01-01","description":"…"}]`},
		{"salary object verbatim", `[{"amount":100000,"currency":"RUR"}]`, `[{"amount":100000,"currency":"RUR"}]`},
	}
	for _, tc := range cases {
		if got := string(editFlatValue(json.RawMessage(tc.in))); got != tc.want {
			t.Fatalf("%s: editFlatValue(%s) = %s, want %s", tc.name, tc.in, got, tc.want)
		}
	}
}

// appliedEqual folds the live hh canonicalizations (2026-09-29): case and
// dash/space variants of dictionary strings, dictionary reorder of string
// lists — while texts stay verbatim and objects compare strictly.
func TestAppliedEqualNormalizations(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"verbatim text", `"Backend (Go)"`, `"Backend (Go)"`, true},
		{"text differs", `"Backend (Go)"`, `"Backend (Go, Python)"`, false},
		{"dictionary case", `["GitLab"]`, `["Gitlab"]`, true},
		{"dash vs space", `["Docker-compose"]`, `["Docker Compose"]`, true},
		{"dictionary expansion", `["HashiCorp Vault"]`, `["Vault"]`, true},
		{"dropped value", `["Terraform"]`, `["Vault"]`, false},
		{"reordered list", `["A","B","C"]`, `["C","A","B"]`, true},
		{"list content differs", `["A","B"]`, `["A","C"]`, false},
		{"empty lists", `[]`, `[]`, true},
		{"experience objects strict", `[{"id":1,"description":"x "}]`, `[{"id":1,"description":"x "}]`, true},
		{"experience description differs", `[{"id":1,"description":"x"}]`, `[{"id":1,"description":"y"}]`, false},
		{"re-forked id is covered", `[{"id":99,"description":"x"}]`, `[{"id":1,"description":"x"}]`, true},
		{"reordered records", `[{"id":2,"description":"b"},{"id":1,"description":"a"}]`, `[{"id":1,"description":"a"},{"id":2,"description":"b"}]`, true},
	}
	for _, tc := range cases {
		if got := appliedEqual(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
			t.Fatalf("%s: appliedEqual(%s, %s) = %t, want %t", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// The verify read-back is the gate: when hh accepts the POST but the value
// did not land, the update is a failure (never a silent no-op).
func TestUpdateResumeContentVerifyMismatch(t *testing.T) {
	origBudget, origPoll := verifyWaitBudget, verifyPollInterval
	defer func() { verifyWaitBudget, verifyPollInterval = origBudget, origPoll }()
	verifyWaitBudget = 50 * time.Millisecond
	verifyPollInterval = 10 * time.Millisecond
	env := newTestEnv(t)
	surface := &contentSurface{state: contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию"), apply: false}
	surface.mount(env)
	c := contentEndpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	mutated := mutatedContent(base, "DevOps-инженер Test (копия)")
	err = c.UpdateResumeContent(context.Background(), "cl0ne1", mutated, []string{"title"})
	if err == nil {
		t.Fatal("verify mismatch must fail the update")
	}
	var ve *ResumeContentVerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T (%v), want *ResumeContentVerifyError", err, err)
	}
	if len(ve.Keys) == 0 || ve.Keys[0] != "resume.title" {
		t.Fatalf("verify keys = %v, want resume.title", ve.Keys)
	}
}

// Validation/business rejections come back typed with the field codes so
// the feature can retry with a fixed value or skip the field.
func TestUpdateResumeContentValidationTyped(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию")))
	})
	env.serve(env.mux1, "1", "POST", "/applicant/resume/edit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"errors":[{"field":"title","fieldNamePath":"/title","type":"BUSINESS","code":"DUPLICATE","message":"занято"}]}`))
	})
	c := contentEndpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	mutated := mutatedContent(base, "Другое")
	err = c.UpdateResumeContent(context.Background(), "cl0ne1", mutated, []string{"title"})
	var ve *ResumeUpdateValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T (%v), want *ResumeUpdateValidationError", err, err)
	}
	if len(ve.Codes) != 1 || ve.Codes[0] != "title:DUPLICATE" {
		t.Fatalf("codes = %v, want [title:DUPLICATE]", ve.Codes)
	}
}

// No diff against the live state means no POST at all (idempotent apply).
func TestUpdateResumeContentNoDiffSkipsPosts(t *testing.T) {
	env := newTestEnv(t)
	surface := &contentSurface{state: contentFixture("cl0ne1", "DevOps-инженер Test", "Про автоматизацию")}
	surface.mount(env)
	c := contentEndpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	if err := c.UpdateResumeContent(context.Background(), "cl0ne1", base, []string{"title"}); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}
	if surface.updateCount() != 0 {
		t.Fatal("an unchanged content must not post any update")
	}
}
