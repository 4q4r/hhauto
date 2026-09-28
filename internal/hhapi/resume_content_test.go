package hhapi

// T29 resume content mutation tests: the builder update payload is the
// SPA-exact serialization reverse-engineered from the deployed
// resume_profile_front bundle (chunk 6069 module 11154 thunk P + cleanForm
// module 867 + hM split module 64673, live build 2026-09-25):
//
//	POST <BFF>/profile/shards/resume/update
//	{resumeHash, currentScreenId, resume, profile, questionToAnswerMap,
//	 additionalProperties:{anyJob:false}, userSkillLevels:{}, userPrint:{}}
//
// with the same header set that unblocked clone_resumes (T28). T27 sent
// the split without additionalProperties.anyJob / userSkillLevels /
// userPrint and through the wrong header set — every shape got a bare 400.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
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
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/update", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body := env.lastBody("1:POST /profile/shards/resume/update")
		s.bodies = append(s.bodies, body)
		// The SPA-exact header set (T28 lesson): without it the service
		// answers a bare 400 for any body.
		if r.Header.Get("X-hhtmSource") == "" || r.Header.Get("X-Requested-With") == "" || r.Header.Get("X-Xsrftoken") == "" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Origin") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s.apply {
			s.state = applyTitle(s.state, body)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null}`))
	})
}

// applyTitle rewrites the served fixture's title when the update body
// carries the new one — the fake hh apply.
func applyTitle(state, body string) string {
	var p struct {
		Resume struct {
			Title json.RawMessage `json:"title"`
		} `json:"resume"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil || len(p.Resume.Title) == 0 {
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
	if string(resume["title"]) == string(p.Resume.Title) {
		return state
	}
	resume["title"] = p.Resume.Title
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
	eps.Override["resume_update"] = env.srv1.URL + "/profile/shards/resume/update"
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

// T29: UpdateResumeContent posts the SPA-exact payload (every key of thunk
// P) with the T28 header set and verifies the change by read-back.
func TestUpdateResumeContentPostsSPAPayload(t *testing.T) {
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
	body := surface.lastUpdate()
	for _, want := range []string{
		`"resumeHash":"cl0ne1"`,
		`"currentScreenId":"title"`,
		`"questionToAnswerMap":{}`,
		`"additionalProperties":{"anyJob":false}`,
		`"userSkillLevels":{}`,
		`"userPrint":{}`,
		`"title":[{"string":"DevOps-инженер Test (копия)"}]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("update body %s misses %s", body, want)
		}
	}
	if strings.Contains(body, "_attributes") || strings.Contains(body, "_conditions") {
		t.Fatal("identity metadata must not travel with the content")
	}
}

// The verify read-back is the gate: when hh accepts the POST but the value
// did not land, the update is a failure (never a silent no-op).
func TestUpdateResumeContentVerifyMismatch(t *testing.T) {
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
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/update", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"errors":[{"field":"title","type":"BUSINESS","code":"DUPLICATE","message":"занято"}]}`))
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
