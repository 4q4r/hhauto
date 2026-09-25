package hhapi

// T29 resume management API tests (reverse-engineered from the deployed
// applicant_profile bundle 2026-09-25, module 91062 — the My-resumes
// delete/hide modal thunks):
//
//	hide:  POST /profile/shards/resume/visibility/hide  {hash, userPrint}
//	show:  POST /profile/shards/resume/visibility/show  {hash, userPrint}
//	delete:POST /profile/shards/resume/delete           {hash, from, hhtmSource}
//	       → 200 {"url": "<redirect>"} (the JS navigates to it)
//
// All three live on the hh.ru site host (the applicant_profile SPA has no
// BFF baseURL override — unlike the resume-profile-front editor POSTs).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func serveManageSurface(env *testEnv) (bodies *[]string, routes *[]string) {
	bodies, routes = &[]string{}, &[]string{}
	record := func(route string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*bodies = append(*bodies, env.lastBody("1:POST "+route))
			*routes = append(*routes, r.Header.Get("X-Xsrftoken"))
			h(w, r)
		}
	}
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/visibility/hide", record("/profile/shards/resume/visibility/hide", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`null`))
	}))
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/visibility/show", record("/profile/shards/resume/visibility/show", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`null`))
	}))
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/delete", record("/profile/shards/resume/delete", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"url":"/applicant/resumes"}`))
	}))
	return bodies, routes
}

func manageClient(env *testEnv) *Client {
	env.t.Helper()
	c, err := NewClient(env.account(), env.endpoints(), nil, nil)
	if err != nil {
		env.t.Fatalf("NewClient: %v", err)
	}
	c.setXSRFForTests("tok_test")
	return c
}

func TestHideResumePostsHashAndUserPrint(t *testing.T) {
	env := newTestEnv(t)
	bodies, _ := serveManageSurface(env)
	c := manageClient(env)
	c.setXSRFForTests("tok_test")

	if err := c.HideResume(context.Background(), "cl0ne1"); err != nil {
		t.Fatalf("HideResume: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("hide POST count = %d", len(*bodies))
	}
	body := (*bodies)[0]
	for _, want := range []string{`"hash":"cl0ne1"`, `"userPrint":{}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("hide body %s misses %s", body, want)
		}
	}
}

func TestShowResumePostsHashAndUserPrint(t *testing.T) {
	env := newTestEnv(t)
	bodies, _ := serveManageSurface(env)
	c := manageClient(env)
	c.setXSRFForTests("tok_test")

	if err := c.ShowResume(context.Background(), "cl0ne1"); err != nil {
		t.Fatalf("ShowResume: %v", err)
	}
	if len(*bodies) != 1 || !strings.Contains((*bodies)[0], `"hash":"cl0ne1"`) {
		t.Fatalf("show bodies = %v", *bodies)
	}
}

func TestDeleteResumePostsFromAndParsesRedirect(t *testing.T) {
	env := newTestEnv(t)
	bodies, _ := serveManageSurface(env)
	c := manageClient(env)
	c.setXSRFForTests("tok_test")

	if err := c.DeleteResume(context.Background(), "sh3ll9"); err != nil {
		t.Fatalf("DeleteResume: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("delete POST count = %d", len(*bodies))
	}
	body := (*bodies)[0]
	for _, want := range []string{`"hash":"sh3ll9"`, `"from":"resume-delete"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("delete body %s misses %s", body, want)
		}
	}
	var p struct {
		From       string `json:"from"`
		HhtmSource string `json:"hhtmSource"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode delete body: %v", err)
	}
	if p.From != "resume-delete" || p.HhtmSource == "" {
		t.Fatalf("delete body fields: from=%q hhtmSource=%q", p.From, p.HhtmSource)
	}
}

// The live contract: management mutations carry the session XSRF token —
// the fake records the X-Xsrftoken header of every POST.
func TestManageMutationsCarryXSRFToken(t *testing.T) {
	env := newTestEnv(t)
	_, tokens := serveManageSurface(env)
	c := manageClient(env)

	if err := c.HideResume(context.Background(), "cl0ne1"); err != nil {
		t.Fatalf("HideResume: %v", err)
	}
	if len(*tokens) != 1 || (*tokens)[0] != "tok_test" {
		t.Fatalf("hide xsrf tokens = %v", *tokens)
	}
}
