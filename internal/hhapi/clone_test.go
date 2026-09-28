package hhapi

// T45: CloneResume is native-only (one clone_resumes call). These tests pin
// the typed rejection surface riding that endpoint: resume-limit re-tagging,
// captcha/logout classification, terminal definitive rejects (no shell
// fallback), the config override key, and the never-auto-retried mutation
// semantics. The full happy path lives in clone_native_test.go.

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// serveCloneRejection wires the parent builder state and makes the
// clone_resumes POST answer with the given status/body. Discovery surfaces
// are registered too (unused when the POST rejects first).
func (env *testEnv) serveCloneRejection(status int, body string) *Client {
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Query().Get("resumeHash") == "p4rent" {
			_, _ = w.Write([]byte(cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", nil)))
			return
		}
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null,"screens":[],"resume":{"_attributes":{"hash":"n4t1v3cl0ne","status":"new","isSearchable":true},"title":[{"string":"DevOps-инженер Test (копия)"}]}}`))
	})
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(cloneShardFixture("285632195", "n4t1v3cl0ne", 0)))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, status)
	})
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")
	return c
}

// A resume-quota verdict on clone_resumes must surface as ErrResumeLimit
// (callers park the clone loop on it). Live rejection shapes are unknown —
// the marker scan covers 403 and 400 validation envelopes.
func TestCloneResumeResumeLimitTyped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"403 limit", http.StatusForbidden, `{"error":"resume limit exceeded"}`},
		{"400 limit", http.StatusBadRequest, `{"errors":["Превышен лимит резюме"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			c := env.serveCloneRejection(tc.status, tc.body)
			_, err := c.CloneResume(context.Background(), "p4rent")
			if !errors.Is(err, ErrResumeLimit) {
				t.Fatalf("err = %v, want ErrResumeLimit", err)
			}
			if env.hits("1:POST /profile/shards/resume/clone_resumes") != 1 {
				t.Fatalf("clone_resumes hits = %d, want 1", env.hits("1:POST /profile/shards/resume/clone_resumes"))
			}
		})
	}
}

// Anti-bot classification rides the native endpoint: a captcha body is a
// typed ErrCaptcha, a login redirect a typed ErrLoggedOut — never a silent
// failure or a fallback attempt.
func TestCloneResumeCaptchaAndLoginTyped(t *testing.T) {
	t.Run("captcha", func(t *testing.T) {
		env := newTestEnv(t)
		c := env.serveCloneRejection(http.StatusForbidden, "showcaptcha")
		_, err := c.CloneResume(context.Background(), "p4rent")
		if !errors.Is(err, ErrCaptcha) {
			t.Fatalf("err = %v, want ErrCaptcha", err)
		}
	})
	t.Run("logged out", func(t *testing.T) {
		env := newTestEnv(t)
		env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", nil)))
		})
		env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/account/login", http.StatusFound)
		})
		c := nativeEndpoints(env)
		c.setXSRFForTests("tok_test")
		_, err := c.CloneResume(context.Background(), "p4rent")
		if !errors.Is(err, ErrLoggedOut) {
			t.Fatalf("err = %v, want ErrLoggedOut", err)
		}
	})
}

// A definitive native rejection (live legacy envelope) is terminal: no shell
// fallback runs, no entity is created elsewhere, and the error wraps
// ErrCloneNative.
func TestCloneResumeNativeRejectIsTerminal(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", nil)))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"@code":"403","$":"Employer mismatch for 0"}`))
	})
	// The removed shell surface: any hit here is a T45 regression. The route
	// is deliberately unregistered — the client must never even call it.
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("shell clone endpoint was called after the T45 removal")
	})
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if hash != "" {
		t.Fatalf("hash = %q on a terminal reject, want empty", hash)
	}
	if !errors.Is(err, ErrCloneNative) {
		t.Fatalf("err = %v, want ErrCloneNative wrap", err)
	}
	if env.hits("1:POST /applicant/resumes/clone") != 0 {
		t.Fatal("the shell endpoint must never be touched")
	}
	if env.hits("1:POST /profile/shards/resume/update") != 0 {
		t.Fatal("no builder update may ride a rejected native clone")
	}
}

// A malformed 200 answer from clone_resumes is a typed native rejection —
// never a silent success and never a hash.
func TestCloneResumeMalformed200IsNativeReject(t *testing.T) {
	for name, body := range map[string]string{
		"not json":  `<html>gateway noise</html>`,
		"builder":   `{"error":[{"key":"internal"}]}`,
		"bad field": `{"errors":[{"field":"roleIds","code":"INVALID"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write([]byte(cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", nil)))
			})
			env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write([]byte(body))
			})
			c := nativeEndpoints(env)
			c.setXSRFForTests("tok_test")
			hash, err := c.CloneResume(context.Background(), "p4rent")
			if hash != "" || err == nil {
				t.Fatalf("CloneResume = %q, %v — want a typed error (no silent success)", hash, err)
			}
			if !errors.Is(err, ErrCloneNative) {
				t.Fatalf("err = %v, want ErrCloneNative wrap", err)
			}
		})
	}
}

// The clone_resumes endpoint is config-overridable ([api.endpoints.override]
// resume_clone_resumes): the override URL is used verbatim and the default
// path is never touched.
func TestCloneResumeEndpointOverride(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Query().Get("resumeHash") == "p4rent" {
			_, _ = w.Write([]byte(cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", nil)))
			return
		}
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null,"screens":[],"resume":{"_attributes":{"hash":"0verr1de","status":"new","isSearchable":true},"title":[{"string":"DevOps-инженер Test (копия)"}]}}`))
	})
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(cloneShardFixture("285632195", "0verr1de", 0)))
	})
	env.serve(env.mux1, "1", "POST", "/custom/clone_resumes", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Xsrftoken") == "" || r.Header.Get("X-hhtmSource") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	// The default path must stay untouched.
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("default clone_resumes URL used despite the override")
	})
	cfg := env.endpoints()
	cfg.Override["resume_clone_resumes"] = env.srv1.URL + "/custom/clone_resumes"
	c, err := NewClient(env.account(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "0verr1de" {
		t.Fatalf("hash = %q, want the clone discovered behind the override", hash)
	}
	if env.hits("1:POST /custom/clone_resumes") != 1 {
		t.Fatalf("override hits = %d, want 1", env.hits("1:POST /custom/clone_resumes"))
	}
}

// clone_resumes is a mutation: an HTTP-level rejection is never auto-retried
// (only a title DUPLICATE bumps the title inside the same call).
func TestCloneResumeMutationIsNeverRetried(t *testing.T) {
	env := newTestEnv(t)
	c := env.serveCloneRejection(http.StatusForbidden, `{"error":"resume limit exceeded"}`)
	_, err := c.CloneResume(context.Background(), "p4rent")
	if !errors.Is(err, ErrResumeLimit) {
		t.Fatalf("err = %v, want ErrResumeLimit", err)
	}
	if env.hits("1:POST /profile/shards/resume/clone_resumes") != 1 {
		t.Fatalf("clone_resumes hits = %d, want 1 (mutations are never auto-retried)", env.hits("1:POST /profile/shards/resume/clone_resumes"))
	}
}
