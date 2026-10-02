package hhapi

// T47 resume-creation tests (live contract decided 2026-09-28):
//
//	POST https://resume-profile-front.hh.ru/profile/shards/resume/create
//	  {"resumesCreationProperties":[{"title":"…","roleIds":[…]}],"userPrint":{}}
//	  (or just {"userPrint":{}} for the empty shell)
//	  → 200 {"nextIncompleteScreenId":"professional_role","screens":[…]}
//
// The envelope carries NO hash, so the new resume is verified through the
// resumes shard diff (the only live-proof method — the probe showed every
// 2xx create adds exactly one not_finished shell, while hh IGNORES any
// content fields: title/roleIds ride the wizard, not create).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// serveCreateSurface registers the create route plus a shard that grows by
// one shell entry from the second read on. Returns the captured bodies and
// the X-Xsrftoken header of every create POST.
func serveCreateSurface(env *testEnv, shellHash string) (*[]string, *[]string) {
	bodies := &[]string{}
	tokens := &[]string{}
	reads := 0
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_abc123", Path: "/"})
		reads++
		body := readLive(env.t, "resumes.json")
		if reads >= 2 {
			body = appendShellEntry(env.t, body, shellHash)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/create", func(w http.ResponseWriter, r *http.Request) {
		*bodies = append(*bodies, env.lastBody("1:POST /profile/shards/resume/create"))
		*tokens = append(*tokens, r.Header.Get("X-Xsrftoken"))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":"professional_role","screens":[{"screenType":"STATIC","screenId":"professional_role","screenContent":[],"messages":[]}]}`))
	})
	return bodies, tokens
}

// appendShellEntry parses the shard fixture, appends one not_finished shell
// and re-marshals — mirror of the live post-create shard state.
func appendShellEntry(t *testing.T, shard []byte, shellHash string) []byte {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(shard, &payload); err != nil {
		t.Fatalf("decode shard fixture: %v", err)
	}
	var resumes []json.RawMessage
	if err := json.Unmarshal(payload["resumes"], &resumes); err != nil {
		t.Fatalf("decode shard resumes: %v", err)
	}
	shell := map[string]any{
		"_attributes": map[string]any{
			"hash":     shellHash,
			"hhid":     "10000001",
			"status":   "not_finished",
			"percent":  0,
			"created":  1790572800000,
			"updated":  1790572800000,
			"modified": "2026-09-28T00:00:00+03:00",
		},
		"title": []any{},
	}
	raw, err := json.Marshal(shell)
	if err != nil {
		t.Fatalf("encode shell: %v", err)
	}
	resumes = append(resumes, raw)
	payload["resumes"], _ = json.Marshal(resumes)
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode shard: %v", err)
	}
	return out
}

func createClient(env *testEnv) *Client {
	env.t.Helper()
	// T50/T82 harness: the tls-client transport requires a captured
	// browser TLS spec — every test client carries the captureSpec stub.
	c, err := NewClient(env.account(), env.endpoints(), nil, nil, WithBrowserTLSSpec(captureSpec(env.t)))
	if err != nil {
		env.t.Fatalf("NewClient: %v", err)
	}
	c.setXSRFForTests("tok_test")
	return c
}

func TestCreateResumeEmptyShellVerifiesThroughShardDiff(t *testing.T) {
	env := newTestEnv(t)
	const shell = "c0ffee00ff112c34820039ed1f60001112223344"
	bodies, _ := serveCreateSurface(env, shell)
	c := createClient(env)

	hash, err := c.CreateResume(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("CreateResume: %v", err)
	}
	if hash != shell {
		t.Fatalf("CreateResume hash = %q, want the fresh shell %q", hash, shell)
	}
	if len(*bodies) != 1 {
		t.Fatalf("create POST count = %d", len(*bodies))
	}
	body := (*bodies)[0]
	if !strings.Contains(body, `"userPrint":{}`) {
		t.Fatalf("create body %s misses userPrint", body)
	}
	if strings.Contains(body, "resumesCreationProperties") {
		t.Fatalf("empty create must not carry creationProperties: %s", body)
	}
}

func TestCreateResumeSendsTitleAndRoleIDs(t *testing.T) {
	env := newTestEnv(t)
	const shell = "baadf00dff112c34820039ed1f60001112223344"
	bodies, _ := serveCreateSurface(env, shell)
	c := createClient(env)

	if _, err := c.CreateResume(context.Background(), "Инженер автоматизации", []string{"160"}); err != nil {
		t.Fatalf("CreateResume: %v", err)
	}
	body := (*bodies)[0]
	for _, want := range []string{
		`"resumesCreationProperties":[{"title":"Инженер автоматизации","roleIds":["160"]}]`,
		`"userPrint":{}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("create body %s misses %s", body, want)
		}
	}
}

// A 2xx create that never shows up in the shard is a silent drop: the
// client refuses to invent a hash (the T29 fail-safe discipline).
func TestCreateResumeUnverifiableDropErrors(t *testing.T) {
	env := newTestEnv(t)
	// Only the base shard: the create 200s but the shard never grows.
	env.serveBootstrap()
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/create", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":"professional_role","screens":[]}`))
	})
	c := createClient(env)

	hash, err := c.CreateResume(context.Background(), "", nil)
	if err == nil {
		t.Fatalf("CreateResume on a silent drop must error, got hash %q", hash)
	}
	if hash != "" {
		t.Fatalf("error path must not return a hash, got %q", hash)
	}
}

// The request rides the SPA-exact header set on the BFF host: the session
// XSRF token must be present (live create/delete evidence, 2026-09-28).
// The token comes from the session cookie the shard read absorbed — the
// same _xsrf→X-XSRFToken discipline the editor's axios interceptors apply.
func TestCreateResumeCarriesXSRFToken(t *testing.T) {
	env := newTestEnv(t)
	_, tokens := serveCreateSurface(env, "aa11bb22ff112c34820039ed1f60001112223344")
	c := createClient(env)

	if _, err := c.CreateResume(context.Background(), "", nil); err != nil {
		t.Fatalf("CreateResume: %v", err)
	}
	if len(*tokens) != 1 || (*tokens)[0] != "tok_abc123" {
		t.Fatalf("create xsrf tokens = %v", *tokens)
	}
}
