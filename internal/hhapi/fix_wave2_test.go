package hhapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
)

// --- Fix 1: offsite redirect => typed ErrForbidden, no retries, chain kept ---

func TestOffsiteRedirectForbiddenNoRetry(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		// Offsite target: the fence must reject it without dialing.
		http.Redirect(w, r, "https://evil.example.com/account/login", http.StatusFound)
	})
	c := env.client()
	c.setXSRFForTests("tok")
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.ListResumes(ctx)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v (%T), want ErrForbidden", err, err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T, want *StatusError", err)
	}
	if len(se.Chain) != 1 || !strings.Contains(se.Chain[0].Location, "evil.example.com") {
		t.Fatalf("chain = %+v, want the offsite hop recorded", se.Chain)
	}
	if errors.Is(err, ErrNetwork) {
		t.Fatal("offsite fence misclassified as network error")
	}
	if got := env.hits("1:GET /shards/applicant/resumes"); got != 1 {
		t.Fatalf("attempts = %d, want 1 (offsite fence must not retry)", got)
	}
}

// --- Fix 2: Set-Cookie on intermediate redirect hops is absorbed ---

func TestIntermediateHopCookiesAbsorbed(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__ddg8", Value: "hop-sentinel", Path: "/"})
		target := env.srv2.URL + "/search/vacancy?" + r.URL.RawQuery
		http.Redirect(w, r, target, http.StatusFound)
	})
	env.serve(env.mux2, "2", "GET", "/search/vacancy", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if _, err := c.SearchVacancies(ctx, SearchQuery{Text: "go", PerPage: 50}); err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	found := false
	for _, ck := range c.Cookies() {
		if ck.Name == "__ddg8" && ck.Value == "hop-sentinel" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hop cookie __ddg8 lost; cookies = %+v", c.Cookies())
	}
}

// --- Fix 3: _xsrf host priority is deterministic ---

func TestCookieBookXSRFHostPriority(t *testing.T) {
	for round := 0; round < 25; round++ { // map order is random; ranking must not be
		var b cookieBook
		b.absorb(
			&http.Cookie{Name: "_xsrf", Value: "chatik-token", Domain: "chatik.hh.ru", Path: "/"},
			&http.Cookie{Name: "_xsrf", Value: "site-token", Domain: "", Path: "/"},
		)
		if got := b.cookie("_xsrf", "hh.ru"); got != "site-token" {
			t.Fatalf("round %d: cookie = %q, want site-token (site scope wins)", round, got)
		}
	}
	var b2 cookieBook
	b2.absorb(
		&http.Cookie{Name: "_xsrf", Value: "chatik-token", Domain: "chatik.hh.ru", Path: "/"},
		&http.Cookie{Name: "_xsrf", Value: "parent-token", Domain: ".hh.ru", Path: "/"},
	)
	if got := b2.cookie("_xsrf", "taganrog.hh.ru"); got != "parent-token" {
		t.Fatalf("cookie = %q, want parent-token (parent domain beats other host)", got)
	}
	var b3 cookieBook
	b3.absorb(&http.Cookie{Name: "_xsrf", Value: "other-token", Domain: "other.example", Path: "/"})
	if got := b3.cookie("_xsrf", "hh.ru"); got != "other-token" {
		t.Fatalf("cookie = %q, want other-token (fallback to last resort)", got)
	}
}

// --- Fix 4: Retry-After beyond the cap stops retrying, surfaces the wait ---

func Test429RetryAfterBeyondCapStopsRetrying(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "31")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	start := time.Now()
	_, err := c.ListResumes(ctx)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T, want *StatusError", err)
	}
	if se.RetryAfter != 31*time.Second {
		t.Fatalf("RetryAfter = %v, want 31s", se.RetryAfter)
	}
	if got := env.hits("1:GET /shards/applicant/resumes"); got != 1 {
		t.Fatalf("attempts = %d, want 1 (Retry-After beyond cap => give up)", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want fast give-up (no inline 30s sleeps)", elapsed)
	}
}

// --- Fix 5: cfg.Headers must not override User-Agent ---

func TestNewClientRejectsUserAgentHeaderOverride(t *testing.T) {
	base := config.Account{UserAgent: chromeWinUA, RPS: 1}
	if _, err := NewClient(base, config.APIEndpoints{}, nil, nil); err != nil {
		t.Fatalf("baseline NewClient: %v", err)
	}
	for _, key := range []string{"User-Agent", "user-agent", "USER-AGENT"} {
		cfg := base
		cfg.Headers = map[string]string{key: "Spoofed/1.0"}
		if _, err := NewClient(cfg, config.APIEndpoints{}, nil, nil); err == nil {
			t.Fatalf("NewClient with Headers[%q] succeeded, want fail-fast", key)
		}
	}
}

// --- Fix 7: BodySnippet masks sensitive token values ---

func TestSnippetRedactsTokens(t *testing.T) {
	in := `{"xsrfToken":"TOPSECRET12","user":{"_xsrf":"SECRET1","hhtoken":"SECRET3"},"ok":true}`
	out := redactSnippet(in)
	for _, secret := range []string{"TOPSECRET12", "SECRET1", "SECRET3"} {
		if strings.Contains(out, secret) {
			t.Fatalf("redactSnippet leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"xsrfToken":"***"`) {
		t.Fatalf("redactSnippet output = %s, want masked xsrfToken", out)
	}
	plain := `text=go&label=no_agencies&hasTest=true`
	if got := redactSnippet(plain); got != plain {
		t.Fatalf("redactSnippet corrupted plain body: %s", got)
	}
	form := `_xsrf=SECRET4&resume_hash=abc`
	got := redactSnippet(form)
	if strings.Contains(got, "SECRET4") {
		t.Fatalf("redactSnippet leaked form token: %s", got)
	}
	if !strings.Contains(got, "resume_hash=abc") {
		t.Fatalf("redactSnippet dropped non-sensitive field: %s", got)
	}
}
