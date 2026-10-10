package hhapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// T118 job_search_status — donor-derived contract (hh-auto-responses
// main.go:2092–2129), NOT live-verified. The endpoint override pin in
// env.endpoints() keeps every test on the local test server.

func TestJobSearchStatusSuccess(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/profile/shards/user_statuses/job_search_status", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status"); got != "looking_for_offers" {
			t.Errorf("status = %q, want looking_for_offers", got)
		}
		if got := r.Header.Get("X-hhtmSource"); got != "resume_list" {
			t.Errorf("X-hhtmSource = %q, want resume_list", got)
		}
		if got := r.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
			t.Errorf("X-Requested-With = %q, want XMLHttpRequest", got)
		}
		if got := r.Header.Get("X-XSRFToken"); got != "tok_abc123" {
			t.Errorf("X-XSRFToken = %q, want tok_abc123", got)
		}
		if !strings.HasPrefix(r.Header.Get("Accept"), "application/json") {
			t.Errorf("Accept = %q, want application/json", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.SetJobSearchStatus(ctx); err != nil {
		t.Fatalf("SetJobSearchStatus: %v", err)
	}
	if body := env.lastBody("1:POST /profile/shards/user_statuses/job_search_status"); body != "" {
		t.Errorf("body = %q, want empty (donor posts no body)", body)
	}
}

// Donor idempotence probe: a repeated call is another ordinary POST — the
// client must not invent client-side dedup or error on the second call.
// (Server-side idempotence is UNPROVEN — live probe pending.)
func TestJobSearchStatusRepeatCall(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/profile/shards/user_statuses/job_search_status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	for i := 1; i <= 2; i++ {
		if err := c.SetJobSearchStatus(ctx); err != nil {
			t.Fatalf("call %d: SetJobSearchStatus: %v", i, err)
		}
	}
	if got := env.hits("1:POST /profile/shards/user_statuses/job_search_status"); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
}

func TestJobSearchStatusCaptchaJSONVerdict(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/profile/shards/user_statuses/job_search_status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hhcaptcha":{"isBot":true}}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.SetJobSearchStatus(ctx); !errors.Is(err, ErrCaptcha) {
		t.Fatalf("err = %v, want ErrCaptcha", err)
	}
}

func TestJobSearchStatusCaptchaRedirect302(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/profile/shards/user_statuses/job_search_status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/captcha/challenge", http.StatusFound)
	})
	env.serve(env.mux1, "1", "GET", "/captcha/challenge", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "captcha_page.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.SetJobSearchStatus(ctx); !errors.Is(err, ErrCaptcha) {
		t.Fatalf("err = %v, want ErrCaptcha", err)
	}
}
