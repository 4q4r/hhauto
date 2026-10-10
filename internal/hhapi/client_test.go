package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
)

// testEnv wires two httptest servers ("main" and a fake regional mirror)
// plus a client pointed at the main one. Handlers are registered per test;
// every test gets a fresh env (ServeMux forbids duplicate patterns).
type testEnv struct {
	t    *testing.T
	mux1 *http.ServeMux
	mux2 *http.ServeMux
	srv1 *httptest.Server
	srv2 *httptest.Server

	mu     sync.Mutex
	counts map[string]int
	bodies map[string][]string // captured request bodies per route key
	ua     string
	rps    float64
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{
		t:      t,
		mux1:   http.NewServeMux(),
		mux2:   http.NewServeMux(),
		counts: map[string]int{},
		bodies: map[string][]string{},
		ua:     chromeWinUA,
		rps:    50,
	}
	env.srv1 = httptest.NewServer(env.mux1)
	env.srv2 = httptest.NewServer(env.mux2)
	t.Cleanup(func() {
		env.srv1.Close()
		env.srv2.Close()
	})
	return env
}

func (env *testEnv) bump(key string) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.counts[key]++
}

func (env *testEnv) hits(key string) int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.counts[key]
}

func (env *testEnv) capture(key string, r *http.Request) {
	if r.Body == nil {
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	env.bodies[key] = append(env.bodies[key], string(b))
}

func (env *testEnv) lastBody(key string) string {
	env.mu.Lock()
	defer env.mu.Unlock()
	bs := env.bodies[key]
	if len(bs) == 0 {
		return ""
	}
	return bs[len(bs)-1]
}

// serve registers a handler with hit counting and method enforcement.
func (env *testEnv) serve(mux *http.ServeMux, serverTag, method, pattern string, h http.HandlerFunc) {
	key := serverTag + ":" + method + " " + pattern
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		env.bump(key)
		env.capture(key, r)
		h(w, r)
	})
}

// serveSettings registers the settings fixture with session cookies
// (xsrf + DDoS-Guard sentinel), as hh.ru would set them.
func (env *testEnv) serveSettings() {
	env.serve(env.mux1, "1", "GET", "/applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_abc123", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "__ddg8", Value: "geo-sentinel", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(readFixture(env.t, "settings.html"))
	})
}

// serveBootstrap registers the light bootstrap endpoint (the resumes JSON
// shard) with session cookies (xsrf + DDoS-Guard sentinel), as hh.ru sets
// them on shard responses.
func (env *testEnv) serveBootstrap() {
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_abc123", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "__ddg8", Value: "geo-sentinel", Path: "/"})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(readLive(env.t, "resumes.json"))
	})
}

func (env *testEnv) endpoints() config.APIEndpoints {
	return config.APIEndpoints{
		Site:       env.srv1.URL,
		Chatik:     env.srv1.URL,
		SearchPage: env.srv1.URL + "/search/vacancy",
		Override: map[string]string{
			// T28/T51/T47: the clone POST defaults to the external
			// resume-profile-front host, the edit POST to the site host —
			// tests must never reach either. create/delete default to the
			// external BFF host (T47 live verdict). Every env pins them to
			// the test server; unregistered patterns 404.
			"resume_clone_resumes": env.srv1.URL + "/profile/shards/resume/clone_resumes",
			"resume_edit":          env.srv1.URL + "/applicant/resume/edit",
			// T56: the experience-replace POST defaults to the external
			// resume-profile-front host.
			"profile_experience_update": env.srv1.URL + "/profile/shards/profile/update",
			"resume_create":             env.srv1.URL + "/profile/shards/resume/create",
			"resume_delete":             env.srv1.URL + "/profile/shards/resume/delete",
			// T118: the job-search-status POST defaults to the external
			// resume-profile-front host.
			"job_search_status": env.srv1.URL + "/profile/shards/user_statuses/job_search_status",
		},
	}
}

func (env *testEnv) account() config.Account {
	rps := env.rps
	if rps == 0 {
		rps = 50
	}
	return config.Account{DisplayName: "acc1", UserAgent: env.ua, RPS: rps}
}

func (env *testEnv) client() *Client {
	env.t.Helper()
	c, err := NewClient(env.account(), env.endpoints(), nil, nil, WithBrowserTLSSpec(captureSpec(env.t)))
	if err != nil {
		env.t.Fatalf("NewClient: %v", err)
	}
	return c
}

func ctxb(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// setXSRFForTests seeds the cached XSRF token without a bootstrap round-trip.
func (c *Client) setXSRFForTests(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.xsrf = token
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// --- Bootstrap / Me ---

func TestBootstrapSessionExtractsXSRFAndMe(t *testing.T) {
	env := newTestEnv(t)
	env.serveBootstrap()
	env.serveSettings()
	c := env.client()

	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.BootstrapSession(ctx); err != nil {
		t.Fatalf("BootstrapSession: %v", err)
	}
	if got := c.xsrfToken(); got != "tok_abc123" {
		t.Fatalf("xsrfToken = %q, want tok_abc123", got)
	}

	u, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.ID != 100500 || u.Email != "user@example.ru" || u.FullName != "Тест Юзер" {
		t.Fatalf("Me = %+v", u)
	}
}

func TestBootstrapDetectsLoggedOut(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/account/login?backurl=%2Fshards%2Fapplicant%2Fresumes", http.StatusFound)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	err := c.BootstrapSession(ctx)
	if !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("BootstrapSession err = %v, want ErrLoggedOut", err)
	}
}

func TestBootstrapForbiddenCaptcha(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(readFixture(env.t, "captcha_page.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	err := c.BootstrapSession(ctx)
	if !errors.Is(err, ErrCaptcha) {
		t.Fatalf("BootstrapSession err = %v, want ErrCaptcha", err)
	}
}

// The _xsrf cookie is the primary mutation token (live-verified: cookie value
// equals the X-XSRFToken hh accepts); a cached SSR-derived token must not
// shadow it.
func TestXSRFCookieBeatsCachedSSRToken(t *testing.T) {
	env := newTestEnv(t)
	c := env.client()
	c.setXSRFForTests("stale_ssr_token")
	c.book.absorb(&http.Cookie{Name: "_xsrf", Value: "tok_cookie", Domain: "hh.ru", Path: "/"})
	if got := c.xsrfToken(); got != "tok_cookie" {
		t.Fatalf("xsrfToken = %q, want cookie value tok_cookie", got)
	}
}

// Without an _xsrf cookie in the jar, bootstrap falls back to the SSR
// settings-page xsrfToken.
func TestBootstrapXSRFSSRFallback(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resumes":[]}`))
	})
	env.serveSettings()
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.BootstrapSession(ctx); err != nil {
		t.Fatalf("BootstrapSession: %v", err)
	}
	if got := c.xsrfToken(); got != "tok_abc123" {
		t.Fatalf("xsrfToken = %q, want SSR fallback tok_abc123", got)
	}
}

// The bootstrap path is overridable via [api.endpoints.override] bootstrap.
func TestBootstrapEndpointOverride(t *testing.T) {
	env := newTestEnv(t)
	eps := env.endpoints()
	eps.Override = map[string]string{epBootstrap: env.srv1.URL + "/mirror/shards/applicant/resumes"}
	env.serve(env.mux1, "1", "GET", "/mirror/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_override", Path: "/"})
		_, _ = w.Write([]byte(`{"resumes":[]}`))
	})
	c, err := NewClient(env.account(), eps, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.BootstrapSession(ctx); err != nil {
		t.Fatalf("BootstrapSession: %v", err)
	}
	if got := c.xsrfToken(); got != "tok_override" {
		t.Fatalf("xsrfToken = %q, want tok_override (override URL must serve bootstrap)", got)
	}
	if got := env.hits("1:GET /shards/applicant/resumes"); got != 0 {
		t.Fatalf("default bootstrap URL hits = %d, want 0", got)
	}
}

// --- Headers on the wire ---

func TestRequestCarriesUAClientHintsAndLanguage(t *testing.T) {
	env := newTestEnv(t)
	var gotHeader http.Header
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		_, _ = w.Write([]byte(`{"resumes":[]}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if _, err := c.ListResumes(ctx); err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	if got := gotHeader.Get("User-Agent"); got != chromeWinUA {
		t.Fatalf("UA = %q", got)
	}
	if got := gotHeader.Get("Accept-Language"); !strings.HasPrefix(got, "ru-RU") {
		t.Fatalf("Accept-Language = %q", got)
	}
	hints := gotHeader.Get("Sec-Ch-Ua")
	if !strings.Contains(hints, `v="128"`) {
		t.Fatalf("Sec-Ch-Ua = %q, want Chrome 128 brands", hints)
	}
	if got := gotHeader.Get("Sec-Ch-Ua-Platform"); got != `"Windows"` {
		t.Fatalf("Sec-Ch-Ua-Platform = %q", got)
	}
	if got := gotHeader.Get("Sec-Ch-Ua-Mobile"); got != "?0" {
		t.Fatalf("Sec-Ch-Ua-Mobile = %q", got)
	}
	if got := gotHeader.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Fatalf("X-Requested-With = %q", got)
	}
}

func TestMutationCarriesXSRFToken(t *testing.T) {
	env := newTestEnv(t)
	env.serveBootstrap()
	var xsrf string
	env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		xsrf = r.Header.Get("X-XSRFToken")
		_, _ = w.Write(readFixture(t, "apply_success.json"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_ = c.BootstrapSession(ctx)
	ref, err := c.Apply(ctx, ApplyRequest{VacancyID: 777, ResumeHash: "resume_hash_1", Letter: "Привет!"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if xsrf != "tok_abc123" {
		t.Fatalf("X-XSRFToken = %q, want tok_abc123", xsrf)
	}
	if ref.ID != "555001" || ref.Vacancy != 777 {
		t.Fatalf("ref = %+v", ref)
	}
	body := env.lastBody("1:POST /applicant/vacancy_response/popup")
	for _, want := range []string{"resume_hash=resume_hash_1", "vacancy_id=777", "letter="} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply form body %q missing %q", body, want)
		}
	}
}

// --- Error mapping ---

func TestErrorMappingTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"429", 429, `{"error":"limit"}`, ErrRateLimited},
		{"403-captcha", 403, `<form id="showcaptcha">`, ErrCaptcha},
		{"403-plain", 403, `{"error":"forbidden"}`, ErrForbidden},
		{"400", 400, `{"error":"required"}`, ErrValidation},
		{"404", 404, "nope", ErrNotFound},
		{"500", 500, "boom", ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := env.client()
			ctx, cancel := ctxb(t)
			defer cancel()
			_, err := c.ListResumes(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v (%T), want errors.Is %v", err, err, tc.want)
			}
		})
	}
}

// --- Retry policy ---

func TestGETRetriesOn5xxThenSucceeds(t *testing.T) {
	env := newTestEnv(t)
	calls := 0
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(readFixture(t, "resumes.json"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	if len(resumes) != 1 {
		t.Fatalf("resumes = %+v", resumes)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestGETRetriesOn429HonoringRetryAfter(t *testing.T) {
	env := newTestEnv(t)
	calls := 0
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"resumes":[]}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	start := time.Now()
	if _, err := c.ListResumes(ctx); err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("elapsed = %v, want >= 1s (Retry-After)", elapsed)
	}
}

func TestApplyPOSTIsNeverRetriedOnGatewayErrors(t *testing.T) {
	for _, status := range []int{502, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			env := newTestEnv(t)
			env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})
			c := env.client()
			ctx, cancel := ctxb(t)
			defer cancel()
			_, err := c.Apply(ctx, ApplyRequest{VacancyID: 777, ResumeHash: "resume_hash_1"})
			var amb *AmbiguousError
			if !errors.As(err, &amb) {
				t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
			}
			if got := env.hits("1:POST /applicant/vacancy_response/popup"); got != 1 {
				t.Fatalf("POST attempts = %d, want 1 (no auto-retry)", got)
			}
		})
	}
}

func TestApplyPOSTValidationErrorNotAmbiguous(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"required":"letter"}}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.Apply(ctx, ApplyRequest{VacancyID: 777, ResumeHash: "resume_hash_1"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	var amb *AmbiguousError
	if errors.As(err, &amb) {
		t.Fatal("422 must not be AmbiguousError (server answered definitively)")
	}
}

func TestApplyNegotiationsLimit(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"negotiationsLimitExceeded":true}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.Apply(ctx, ApplyRequest{VacancyID: 777, ResumeHash: "resume_hash_1"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestApplyRedirectToLimitPage(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/applicant/negotiations-limit-exceeded?from=popup", http.StatusFound)
	})
	env.serve(env.mux1, "1", "GET", "/applicant/negotiations-limit-exceeded", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>negotiations-limit-exceeded</body></html>`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.Apply(ctx, ApplyRequest{VacancyID: 777, ResumeHash: "resume_hash_1"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// --- Captcha ---

func TestTouchCaptchaRedirect302(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/touch", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/captcha/challenge", http.StatusFound)
	})
	env.serve(env.mux1, "1", "GET", "/captcha/challenge", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "captcha_page.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	err := c.TouchResume(ctx, "resume_hash_1")
	if !errors.Is(err, ErrCaptcha) {
		t.Fatalf("err = %v, want ErrCaptcha", err)
	}
}

func TestTouchCaptchaJSONVerdict(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/touch", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hhcaptcha":{"isBot":true}}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.TouchResume(ctx, "resume_hash_1"); !errors.Is(err, ErrCaptcha) {
		t.Fatalf("err = %v, want ErrCaptcha", err)
	}
}

func TestTouchSuccess(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/touch", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-XSRFToken"); got != "tok_abc123" {
			t.Errorf("X-XSRFToken = %q, want tok_abc123", got)
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.TouchResume(ctx, "resume_hash_1"); err != nil {
		t.Fatalf("TouchResume: %v", err)
	}
}

// T114 golden form: the touch body carries undirectable=true next to the
// resume hash (donor contract: hh must not redirect the resume to another
// position when lifting). url.Values.Encode() sorts keys, so the body is
// deterministic.
func TestTouchGoldenForm(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/touch", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.TouchResume(ctx, "resume_hash_1"); err != nil {
		t.Fatalf("TouchResume: %v", err)
	}
	if got, want := env.lastBody("1:POST /applicant/resumes/touch"), "resume=resume_hash_1&undirectable=true"; got != want {
		t.Fatalf("touch body = %q, want %q", got, want)
	}
}

// T114: 409 means the hh 4h server timer has not elapsed — a distinct
// sentinel, checked before the captcha markers (which ride a 200 today).
func TestTouchTooEarly409(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/applicant/resumes/touch", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"too_early"}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	err := c.TouchResume(ctx, "resume_hash_1")
	if !errors.Is(err, ErrTouchTooEarly) {
		t.Fatalf("err = %v, want ErrTouchTooEarly", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict {
		t.Fatalf("errors.As(*StatusError): %#v, want StatusCode 409", se)
	}
}

// --- Logged out ---

func TestLoggedOutViaLoginRedirect(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/account/login?backurl=%2Fshards%2Fapplicant%2Fresumes", http.StatusFound)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.ListResumes(ctx)
	if !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("err = %v, want ErrLoggedOut", err)
	}
}

// --- XSRF refresh on 403 ---

func TestXSRFRefreshRetriesMutationOnce(t *testing.T) {
	env := newTestEnv(t)
	env.serveBootstrap()
	calls := 0
	env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-XSRFToken") == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"xsrf token missing"}`))
			return
		}
		_, _ = w.Write(readFixture(t, "apply_success.json"))
	})
	c := env.client() // no bootstrap: token starts empty
	ctx, cancel := ctxb(t)
	defer cancel()
	if _, err := c.Apply(ctx, ApplyRequest{VacancyID: 777, ResumeHash: "resume_hash_1"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if calls != 2 {
		t.Fatalf("apply calls = %d, want 2 (403 then success after xsrf refresh)", calls)
	}
	if got := env.hits("1:GET /shards/applicant/resumes"); got != 1 {
		t.Fatalf("xsrf refreshes = %d, want 1", got)
	}
}

// --- Host pinning via redirects ---

func TestRedirectPinsRegionalHost(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		target := env.srv2.URL + "/search/vacancy?" + r.URL.RawQuery
		http.Redirect(w, r, target, http.StatusFound)
	})
	env.serve(env.mux2, "2", "GET", "/search/vacancy", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	env.serve(env.mux2, "2", "GET", "/vacancy/777001", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "vacancy.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()

	page, err := c.SearchVacancies(ctx, SearchQuery{Text: "go", Page: 0, PerPage: 50})
	if err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	// Pinning: the next site request (vacancy card) must go straight to srv2.
	if _, err := c.VacancyCard(ctx, 777001); err != nil {
		t.Fatalf("VacancyCard: %v", err)
	}
	if got := env.hits("1:GET /search/vacancy"); got != 1 {
		t.Fatalf("srv1 search hits = %d, want 1", got)
	}
	if got := env.hits("2:GET /search/vacancy"); got != 1 {
		t.Fatalf("srv2 search hits = %d, want 1", got)
	}
	if got := env.hits("2:GET /vacancy/777001"); got != 1 {
		t.Fatalf("srv2 vacancy hits = %d, want 1 (host pinned)", got)
	}
	if env.hits("1:GET /vacancy/777001") != 0 {
		t.Fatal("vacancy card went to srv1 despite pinning")
	}
}

// --- SSR parsing ---

func TestSearchVacanciesParamsAndParsing(t *testing.T) {
	env := newTestEnv(t)
	var gotQuery string
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	page, err := c.SearchVacancies(ctx, SearchQuery{
		Text:            `"go backend"`,
		Areas:           []string{"113", "1"},
		Experience:      []string{"between1And3"},
		Schedule:        []string{"fullDay"},
		Employment:      []string{"full"},
		SalaryFrom:      150000,
		Currency:        "RUR",
		LabelNoAgencies: true,
		SearchFields:    []string{"name"},
		PeriodDays:      7,
		Page:            2,
		PerPage:         50,
	})
	if err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	for _, want := range []string{
		"text=%22go+backend%22", // quoting flows through from the query text
		"area=113", "area=1",
		"experience=between1And3", "schedule=fullDay", "employment=full",
		"salary=150000", "currency=RUR", "label=not_from_agency", "search_period=7",
		"page=2", "items_on_page=50",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
	if page.Found != 57 || page.Pages != 2 || page.Page != 2 {
		t.Fatalf("page meta = %+v", page)
	}
	v := page.Items[0]
	if v.ID != 777001 || v.Name != "Backend-разработчик (Go)" || v.EmployerID != 9001 || v.EmployerName != "Corp LLC" {
		t.Fatalf("item0 = %+v", v)
	}
	if v.Salary == nil || v.Salary.From != 150000 || v.Salary.To != 250000 || v.Salary.Currency != "RUR" || !v.Salary.Gross {
		t.Fatalf("item0 salary = %+v", v.Salary)
	}
	if !v.EmployerOnline || !v.HasTests || v.AcceptTemp {
		t.Fatalf("item0 flags = online:%v tests:%v temp:%v", v.EmployerOnline, v.HasTests, v.AcceptTemp)
	}
	if v.AlternateURL == "" {
		t.Fatal("item0 alternate_url empty")
	}
	if v2 := page.Items[1]; v2.AcceptTemp != true || v2.EmployerOnline {
		t.Fatalf("item1 flags = %+v", v2)
	}
}

func TestSearchFields(t *testing.T) {
	env := newTestEnv(t)
	var queries []string
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()

	if _, err := c.SearchVacancies(ctx, SearchQuery{Text: "go", PerPage: 50}); err != nil {
		t.Fatalf("all fields: %v", err)
	}
	if _, err := c.SearchVacancies(ctx, SearchQuery{Text: "go", SearchFields: []string{"name", "description", "company_name"}, PerPage: 50}); err != nil {
		t.Fatalf("named fields: %v", err)
	}
	all, named := queries[0], queries[1]
	if strings.Contains(all, "search_field") {
		t.Fatalf("empty search_fields must not send the param: %q", all)
	}
	if !strings.Contains(named, "search_field=name&search_field=description&search_field=company_name") {
		t.Fatalf("search_fields must repeat the param in order: %q", named)
	}
}

// T22: the SSR search page is a top-level navigation and hh.ru answers
// Accept: application/json with 406 there, so the search request must carry
// the browser text/html form while JSON shards keep their fetch() Accept.
func TestSearchSSRRequestCarriesBrowserAccept(t *testing.T) {
	env := newTestEnv(t)
	var searchAccept, shardAccept string
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		searchAccept = r.Header.Get("Accept")
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		shardAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"resumes":[]}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()

	if _, err := c.SearchVacancies(ctx, SearchQuery{Text: "go", PerPage: 50}); err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	if _, err := c.ListResumes(ctx); err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	const wantDocAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	if searchAccept != wantDocAccept {
		t.Fatalf("search Accept = %q, want %q", searchAccept, wantDocAccept)
	}
	const wantShardAccept = "application/json, text/plain, */*"
	if shardAccept != wantShardAccept {
		t.Fatalf("resumes shard Accept = %q, want %q", shardAccept, wantShardAccept)
	}
}

func TestSuitableVacanciesFromSSR(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "resume_page.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	page, err := c.SuitableVacancies(ctx, "resume_hash_1", 0)
	if err != nil {
		t.Fatalf("SuitableVacancies: %v", err)
	}
	if len(page.Items) != 2 || page.Found != 2 {
		t.Fatalf("page = %+v", page)
	}
}

func TestVacancyCardAndEmployerFromSSR(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/vacancy/777", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Referer"); !strings.HasSuffix(got, "/search/vacancy") {
			t.Errorf("Referer = %q, want search page suffix", got)
		}
		_, _ = w.Write(readFixture(t, "vacancy.html"))
	})
	env.serve(env.mux1, "1", "GET", "/employer/9001", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "employer.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()

	v, err := c.VacancyCard(ctx, 777)
	if err != nil {
		t.Fatalf("VacancyCard: %v", err)
	}
	if v.ID != 777 || v.EmployerID != 9001 || !strings.Contains(v.Description, "postgres") {
		t.Fatalf("vacancy = %+v", v)
	}
	if v.Salary == nil || v.Salary.From != 150000 {
		t.Fatalf("salary = %+v", v.Salary)
	}

	e, err := c.EmployerCard(ctx, 9001)
	if err != nil {
		t.Fatalf("EmployerCard: %v", err)
	}
	if e.ID != 9001 || e.Name != "Corp LLC" || e.SiteURL != "https://corp.example.ru" {
		t.Fatalf("employer = %+v", e)
	}

	contacts, err := c.EmployerContacts(ctx, 9001)
	if err != nil {
		t.Fatalf("EmployerContacts: %v", err)
	}
	if len(contacts) != 2 || contacts[0].Type != "email" || contacts[0].Value != "hr@corp.example.ru" {
		t.Fatalf("contacts = %+v", contacts)
	}
}

func TestEmployerRating(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/employer_reviews/proxy_components/small_widget", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("employerId") != "9001" {
			t.Errorf("employerId = %q", r.URL.Query().Get("employerId"))
		}
		_, _ = w.Write(readFixture(t, "rating.json"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	r, err := c.EmployerRating(ctx, 9001)
	if err != nil {
		t.Fatalf("EmployerRating: %v", err)
	}
	if r.EmployerID != 9001 || r.ScoreAll != 4.3 || r.ScoreRecent != 4.6 {
		t.Fatalf("rating = %+v", r)
	}
	if r.ReviewsCount != 120 || r.RecommendPct != 81 || r.FetchedAt.IsZero() {
		t.Fatalf("rating meta = %+v", r)
	}
}

func TestApplyPopupMeta(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vacancyId") != "777" {
			t.Errorf("vacancyId = %q", r.URL.Query().Get("vacancyId"))
		}
		_, _ = w.Write(readFixture(t, "popup.json"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	meta, err := c.ApplyPopup(ctx, 777)
	if err != nil {
		t.Fatalf("ApplyPopup: %v", err)
	}
	if !meta.LetterRequired || meta.HasTests || !meta.AcceptAutoResponse || meta.ChatWritePossibility != "ENABLED" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.ResumeInconsistencies) != 1 || !strings.Contains(meta.ResumeInconsistencies[0], "EXPERIENCE") {
		t.Fatalf("inconsistencies = %+v", meta.ResumeInconsistencies)
	}
}

func TestNegotiationsFromSSR(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/negotiations", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "negotiations.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	negs, err := c.Negotiations(ctx, 0)
	if err != nil {
		t.Fatalf("Negotiations: %v", err)
	}
	if len(negs) != 1 {
		t.Fatalf("negs = %+v", negs)
	}
	n := negs[0]
	if n.ID != "555001" || n.VacancyID != 777 || n.VacancyName != "Go Backend" || n.EmployerName != "Corp LLC" || n.State != "INVITATION" {
		t.Fatalf("negotiation = %+v", n)
	}
}

// --- Chatik ---

func TestListChatsAndHistory(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/chatik/api/chats", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "" || r.Header.Get("Referer") == "" {
			t.Errorf("chatik list missing Origin/Referer")
		}
		_, _ = w.Write(readFixture(t, "chats.json"))
	})
	env.serve(env.mux1, "1", "GET", "/chatik/api/chat_data", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("chatId") != "555001" {
			t.Errorf("chatId = %q", r.URL.Query().Get("chatId"))
		}
		_, _ = w.Write(readFixture(t, "chat_data.json"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()

	chats, err := c.ListChats(ctx, 0)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != "555001" || chats[0].Unread != 1 || chats[0].EmployerName != "Corp LLC" {
		t.Fatalf("chats = %+v", chats)
	}
	if chats[0].LastMessageAt.IsZero() {
		t.Fatal("LastMessageAt zero")
	}

	msgs, err := c.ChatHistory(ctx, "555001")
	if err != nil {
		t.Fatalf("ChatHistory: %v", err)
	}
	if len(msgs) != 2 { // workflow item skipped
		t.Fatalf("msgs = %+v", msgs)
	}
	if msgs[0].Direction != "in" || msgs[1].Direction != "out" {
		t.Fatalf("directions = %q %q", msgs[0].Direction, msgs[1].Direction)
	}
	if msgs[0].Text != "Здравствуйте!" || msgs[1].ID != "m2" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestSendMessageRetriesWithStableIdempotencyKey(t *testing.T) {
	env := newTestEnv(t)
	calls := 0
	env.serve(env.mux1, "1", "POST", "/chatik/api/send", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(readFixture(t, "send_ok.json"))
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.SendMessage(ctx, "555001", "fixed-key-1", "Привет!"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	bodies := env.bodies["1:POST /chatik/api/send"]
	seenKeys := map[string]bool{}
	for _, b := range bodies {
		var payload struct {
			ChatID         int64  `json:"chatId"`
			IdempotencyKey string `json:"idempotencyKey"`
			Text           string `json:"text"`
		}
		if err := json.Unmarshal([]byte(b), &payload); err != nil {
			t.Fatalf("bad send body %q: %v", b, err)
		}
		if payload.ChatID != 555001 || payload.Text != "Привет!" {
			t.Fatalf("payload = %+v", payload)
		}
		seenKeys[payload.IdempotencyKey] = true
	}
	if len(seenKeys) != 1 || !seenKeys["fixed-key-1"] {
		t.Fatalf("idempotency keys not stable: %v", seenKeys)
	}
}

func TestSendMessage5xxStaysAmbiguousAfterRetries(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/chatik/api/send", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	err := c.SendMessage(ctx, "555001", "k", "x")
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v (%T), want AmbiguousError (message may have landed)", err, err)
	}
	if got := env.hits("1:POST /chatik/api/send"); got != 3 {
		t.Fatalf("attempts = %d, want 3 (idempotent POST retries)", got)
	}
}

func TestLeaveChat(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "POST", "/chatik/api/participant_action", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.LeaveChat(ctx, "555001"); err != nil {
		t.Fatalf("LeaveChat: %v", err)
	}
	body := env.lastBody("1:POST /chatik/api/participant_action")
	if !strings.Contains(body, `"chatId":555001`) || !strings.Contains(body, "LEAVE") {
		t.Fatalf("leave body = %q", body)
	}
}

// --- Cookie persistence ---

type recordingPersister struct {
	mu      sync.Mutex
	saves   [][]*http.Cookie
	account string
}

func (p *recordingPersister) Save(_ context.Context, accountID string, cookies []*http.Cookie) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.account = accountID
	cp := make([]*http.Cookie, len(cookies))
	for i, c := range cookies {
		cc := *c
		cp[i] = &cc
	}
	p.saves = append(p.saves, cp)
	return nil
}

func TestCookiePersistenceRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	var gotXSRF string
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_abc123", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "__ddg8", Value: "geo-sentinel", Path: "/"})
		ck, err := r.Cookie("_xsrf")
		if err == nil {
			gotXSRF = ck.Value
		}
		_, _ = w.Write([]byte(`{"resumes":[]}`))
	})
	c := env.client()
	p := &recordingPersister{}
	c.SetPersister(p, "acc1")

	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.BootstrapSession(ctx); err != nil {
		t.Fatalf("BootstrapSession: %v", err)
	}
	p.mu.Lock()
	total := len(p.saves)
	var last []*http.Cookie
	if total > 0 {
		last = append([]*http.Cookie(nil), p.saves[total-1]...)
	}
	acc := p.account
	p.mu.Unlock()
	if total == 0 {
		t.Fatal("persister never called")
	}
	if acc != "acc1" {
		t.Fatalf("accountID = %q", acc)
	}
	names := map[string]bool{}
	for _, ck := range last {
		names[ck.Name] = true
	}
	if !names["_xsrf"] || !names["__ddg8"] {
		t.Fatalf("persisted cookies %v missing _xsrf/__ddg8 (DDoS-Guard must survive)", names)
	}

	// Fresh client restores the session from the persisted snapshot.
	c2, err := NewClient(env.account(), env.endpoints(), nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c2.LoadCookies(ctx, last); err != nil {
		t.Fatalf("LoadCookies: %v", err)
	}
	if _, err := c2.ListResumes(ctx); err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	if gotXSRF != "tok_abc123" {
		t.Fatalf("restored _xsrf = %q", gotXSRF)
	}
}

// --- Rate limiting ---

func TestLimiterBlocksBursts(t *testing.T) {
	l := newLimiter(25) // 40ms period
	start := time.Now()
	if err := l.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	start = time.Now()
	if err := l.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := time.Since(start)
	if first > 20*time.Millisecond {
		t.Fatalf("first wait = %v, want immediate", first)
	}
	if second < 15*time.Millisecond {
		t.Fatalf("second wait = %v, want ~40ms period", second)
	}
}

func TestDefaultRPSAppliedWhenZero(t *testing.T) {
	l := newLimiter(0)
	if l.limit() != defaultRPS {
		t.Fatalf("limit = %v, want default %v", l.limit(), defaultRPS)
	}
}

// --- Constructor ---

func TestNewClientRequiresUserAgent(t *testing.T) {
	cfg := config.Account{UserAgent: "", RPS: 1}
	if _, err := NewClient(cfg, config.APIEndpoints{}, nil, nil); err == nil {
		t.Fatal("NewClient with empty UA succeeded, want error")
	}
}

func TestCookiesSnapshot(t *testing.T) {
	env := newTestEnv(t)
	env.serveBootstrap()
	c := env.client()
	if len(c.Cookies()) != 0 {
		t.Fatal("fresh client has cookies")
	}
	ctx, cancel := ctxb(t)
	defer cancel()
	if err := c.BootstrapSession(ctx); err != nil {
		t.Fatalf("BootstrapSession: %v", err)
	}
	if len(c.Cookies()) == 0 {
		t.Fatal("Cookies() empty after bootstrap")
	}
}
