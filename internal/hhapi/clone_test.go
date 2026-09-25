package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// T26: CloneResume posts JSON {"resumeId":...} with XHR headers + X-XSRFToken
// and parses the new resume hash from the live-verified response envelope
// {"url":"/profile/resume/?resume=<hash>"} (live-verified 2026-09-25).

func TestCloneResumeParsesNewHash(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-XSRFToken"); got != "tok_test" {
			t.Errorf("X-XSRFToken = %q, want seeded token", got)
		}
		if got := r.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
			t.Errorf("X-Requested-With = %q, want XMLHttpRequest (live XHR shape)", got)
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Errorf("Content-Type = %q, want application/json (live JSON body)", got)
		}
		_, _ = w.Write([]byte(`{"url":"/profile/resume/?resume=n3wh4sh"}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "h4sh1")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "n3wh4sh" {
		t.Fatalf("CloneResume hash = %q, want %q", hash, "n3wh4sh")
	}
	if env.hits("1:POST /applicant/resumes/clone") != 1 {
		t.Fatalf("clone endpoint hits = %d, want 1", env.hits("1:POST /applicant/resumes/clone"))
	}
	var body struct {
		ResumeID string `json:"resumeId"`
	}
	raw := env.lastBody("1:POST /applicant/resumes/clone")
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", raw, err)
	}
	if body.ResumeID != "h4sh1" {
		t.Fatalf("request resumeId = %q, want %q", body.ResumeID, "h4sh1")
	}
}

func TestCloneResumeHashFromAbsoluteURLEnvelope(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"https://hh.ru/profile/resume/?resume=abs0lute&hhtmFrom=resume_list"}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_test")
	hash, err := c.CloneResume(context.Background(), "h4sh1")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "abs0lute" {
		t.Fatalf("hash = %q, want %q (query param of the returned url)", hash, "abs0lute")
	}
}

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
			env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, tc.body, tc.status)
			})
			c := env.client()
			c.setXSRFForTests("tok_test")
			_, err := c.CloneResume(context.Background(), "h4sh1")
			if !errors.Is(err, ErrResumeLimit) {
				t.Fatalf("err = %v, want ErrResumeLimit", err)
			}
		})
	}
}

func TestCloneResumeCaptchaAndLoginTyped(t *testing.T) {
	t.Run("captcha", func(t *testing.T) {
		env := newTestEnv(t)
		env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "showcaptcha", http.StatusForbidden)
		})
		c := env.client()
		c.setXSRFForTests("tok_test")
		_, err := c.CloneResume(context.Background(), "h4sh1")
		if !errors.Is(err, ErrCaptcha) {
			t.Fatalf("err = %v, want ErrCaptcha", err)
		}
	})
	t.Run("logged out", func(t *testing.T) {
		env := newTestEnv(t)
		env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/account/login", http.StatusFound)
		})
		c := env.client()
		c.setXSRFForTests("tok_test")
		_, err := c.CloneResume(context.Background(), "h4sh1")
		if !errors.Is(err, ErrLoggedOut) {
			t.Fatalf("err = %v, want ErrLoggedOut", err)
		}
	})
}

func TestCloneResumeMalformed200IsUpstream(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":  `{}`,
		"url no resume": `{"url":"/applicant/resumes"}`,
		"broken url":    `{"url":"%%%"}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			c := env.client()
			c.setXSRFForTests("tok_test")
			hash, err := c.CloneResume(context.Background(), "h4sh1")
			if err == nil {
				t.Fatalf("CloneResume = %q, want error (no silent success)", hash)
			}
			var se *StatusError
			if !errors.As(err, &se) || !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v, want ErrUpstream StatusError", err)
			}
		})
	}
}

func TestCloneResumeEndpointOverride(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/custom/clone", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"/profile/resume/?resume=0verr1de"}`))
	})
	cfg := env.endpoints()
	cfg.Override = map[string]string{"resume_clone": env.srv1.URL + "/custom/clone"}
	c, err := NewClient(env.account(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.setXSRFForTests("tok_test")
	hash, err := c.CloneResume(context.Background(), "h4sh1")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "0verr1de" || env.hits("1:POST /custom/clone") != 1 {
		t.Fatalf("hash=%q hits=%d, want the override endpoint used", hash, env.hits("1:POST /custom/clone"))
	}
}

func TestCloneResumeMutationIsNeverRetried(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/clone", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"resume limit exceeded"}`, http.StatusForbidden)
	})
	c := env.client()
	c.setXSRFForTests("tok_test")
	_, err := c.CloneResume(context.Background(), "h4sh1")
	if !errors.Is(err, ErrResumeLimit) {
		t.Fatalf("err = %v, want ErrResumeLimit", err)
	}
	if env.hits("1:POST /applicant/resumes/clone") != 1 {
		t.Fatalf("clone endpoint hits = %d, want 1 (mutations are never auto-retried)", env.hits("1:POST /applicant/resumes/clone"))
	}
}
