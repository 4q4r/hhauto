package captcha

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
)

// twoCaptchaFake is an in-process fake of the 2captcha in.php/res.php protocol.
type twoCaptchaFake struct {
	mu sync.Mutex

	submitStatus   int    // status field of the in.php JSON response
	submitReq      string // request field of the in.php JSON response
	pollResponses  []string
	submitCalls    int
	pollCalls      int
	balanceResp    string
	balanceCalls   int
	forceHTTPError int // if >0, res.php replies with this status code

	// observed request data
	lastSubmitMethod string
	lastSubmitForm   map[string]string
	lastPollQuery    map[string]string
}

func (f *twoCaptchaFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/in.php":
		f.submitCalls++
		f.lastSubmitMethod = r.Method
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.lastSubmitForm = formMap(r)
		writeJSON(w, fmt.Sprintf(`{"status":%d,"request":%q}`, f.submitStatus, f.submitReq))
	case "/res.php":
		if f.forceHTTPError > 0 {
			w.WriteHeader(f.forceHTTPError)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.pollCalls++
		f.lastPollQuery = formMap(r)
		action := r.URL.Query().Get("action")
		if action == "getbalance" {
			f.balanceCalls++
			writeJSON(w, fmt.Sprintf(`{"status":1,"request":%q}`, f.balanceResp))
			return
		}
		// action=get: replay scripted poll responses, hold at CAPCHA_NOT_READY.
		resp := "CAPCHA_NOT_READY"
		if len(f.pollResponses) > 0 {
			resp = f.pollResponses[0]
			f.pollResponses = f.pollResponses[1:]
		}
		writeJSON(w, resp)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func formMap(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, vs := range r.Form {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func newTwoCaptchaFake(t *testing.T, f *twoCaptchaFake) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return srv
}

func twoCaptchaCfg(baseURL string) config.CaptchaProvider {
	return config.CaptchaProvider{
		Protocol: "2captcha",
		BaseURL:  baseURL,
		Key:      "test-key",
		PollMS:   1,
		TimeoutS: 5,
	}
}

func solvedImageToken(t *testing.T, s Solver, id string) Solution {
	t.Helper()
	sol, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	if sol.Token == "" || sol.ProviderID != id {
		t.Fatalf("Solve() = %+v, want token with provider %q", sol, id)
	}
	return sol
}

func TestTwoCaptchaImageSolveSubmitPollToken(t *testing.T) {
	fake := &twoCaptchaFake{submitStatus: 1, submitReq: "2122988149", pollResponses: []string{
		`{"status":0,"request":"CAPCHA_NOT_READY"}`,
		`{"status":1,"request":"captured_token"}`,
	}}
	srv := newTwoCaptchaFake(t, fake)

	s, err := newTwoCaptcha("2c-main", twoCaptchaCfg(srv.URL))
	if err != nil {
		t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
	}

	sol := solvedImageToken(t, s, "2c-main")
	if sol.Token != "captured_token" {
		t.Fatalf("token = %q, want %q", sol.Token, "captured_token")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.submitCalls != 1 || fake.pollCalls != 2 {
		t.Fatalf("submit=%d poll=%d, want 1 and 2", fake.submitCalls, fake.pollCalls)
	}
	if fake.lastSubmitMethod != http.MethodPost {
		t.Fatalf("in.php method = %q, want POST (key must not leak into URLs)", fake.lastSubmitMethod)
	}
	want := map[string]string{"key": "test-key", "json": "1", "method": "base64", "body": "aGVsbG8="}
	for k, v := range want {
		if got := fake.lastSubmitForm[k]; got != v {
			t.Fatalf("in.php form[%q] = %q, want %q", k, got, v)
		}
	}
	if got := fake.lastPollQuery["action"]; got != "get" {
		t.Fatalf("res.php action = %q, want get", got)
	}
	if got := fake.lastPollQuery["id"]; got != "2122988149" {
		t.Fatalf("res.php id = %q, want 2122988149", got)
	}
	if got := fake.lastPollQuery["key"]; got != "test-key" {
		t.Fatalf("res.php key mismatch: %q", got)
	}
}

func TestTwoCaptchaHCaptchaAndTurnstileParams(t *testing.T) {
	cases := []struct {
		name       string
		ch         Challenge
		wantMethod string
	}{
		{
			name:       "hcaptcha",
			ch:         Challenge{Type: TypeHCaptcha, SiteKey: "10000000-ffff-ffff-ffff-000000000001", PageURL: "https://hh.ru/auth"},
			wantMethod: "hcaptcha",
		},
		{
			name:       "turnstile",
			ch:         Challenge{Type: TypeTurnstile, SiteKey: "0x4AAAAAAAC3DHQFLr1GavRN", PageURL: "https://hh.ru/search"},
			wantMethod: "turnstile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &twoCaptchaFake{submitStatus: 1, submitReq: "777", pollResponses: []string{
				`{"status":1,"request":"P0_token"}`,
			}}
			srv := newTwoCaptchaFake(t, fake)

			s, err := newTwoCaptcha("2c-hcap", twoCaptchaCfg(srv.URL))
			if err != nil {
				t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
			}

			sol, err := s.Solve(context.Background(), tc.ch)
			if err != nil {
				t.Fatalf("Solve() error = %v, want nil", err)
			}
			if sol.Token != "P0_token" {
				t.Fatalf("token = %q, want P0_token", sol.Token)
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()
			form := fake.lastSubmitForm
			if form["method"] != tc.wantMethod {
				t.Fatalf("method = %q, want %q", form["method"], tc.wantMethod)
			}
			if form["sitekey"] != tc.ch.SiteKey || form["pageurl"] != tc.ch.PageURL {
				t.Fatalf("sitekey/pageurl = %q/%q, want %q/%q", form["sitekey"], form["pageurl"], tc.ch.SiteKey, tc.ch.PageURL)
			}
		})
	}
}

func TestTwoCaptchaSubmitProtocolError(t *testing.T) {
	fake := &twoCaptchaFake{submitStatus: 0, submitReq: "ERROR_KEY_DOES_NOT_EXIST"}
	srv := newTwoCaptchaFake(t, fake)

	s, err := newTwoCaptcha("2c-bad", twoCaptchaCfg(srv.URL))
	if err != nil {
		t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
	}

	_, err = s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("Solve() error = %v (%T), want *ProtocolError", err, err)
	}
	if pe.Code != "ERROR_KEY_DOES_NOT_EXIST" {
		t.Fatalf("ProtocolError.Code = %q, want ERROR_KEY_DOES_NOT_EXIST", pe.Code)
	}
	if pe.ProviderID != "2c-bad" {
		t.Fatalf("ProtocolError.ProviderID = %q, want 2c-bad", pe.ProviderID)
	}
}

func TestTwoCaptchaUnsolvableDuringPoll(t *testing.T) {
	fake := &twoCaptchaFake{submitStatus: 1, submitReq: "42", pollResponses: []string{
		`{"status":0,"request":"ERROR_CAPTCHA_UNSOLVABLE"}`,
	}}
	srv := newTwoCaptchaFake(t, fake)

	s, _ := newTwoCaptcha("2c-unsolv", twoCaptchaCfg(srv.URL))

	_, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "ERROR_CAPTCHA_UNSOLVABLE" {
		t.Fatalf("Solve() error = %v, want ProtocolError(ERROR_CAPTCHA_UNSOLVABLE)", err)
	}
}

func TestTwoCaptchaSolveTimeout(t *testing.T) {
	// res.php never becomes ready.
	fake := &twoCaptchaFake{submitStatus: 1, submitReq: "43"}
	srv := newTwoCaptchaFake(t, fake)

	s, _ := newTwoCaptcha("2c-slow", twoCaptchaCfg(srv.URL))
	s.pollInterval = time.Millisecond
	s.solveTimeout = 30 * time.Millisecond

	start := time.Now()
	_, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	if err == nil {
		t.Fatal("Solve() error = nil, want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Solve() took %v, want ~solveTimeout", elapsed)
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatalf("timeout error leaks API key: %v", err)
	}
}

func TestTwoCaptchaBalance(t *testing.T) {
	fake := &twoCaptchaFake{balanceResp: "9.99"}
	srv := newTwoCaptchaFake(t, fake)

	s, _ := newTwoCaptcha("2c-bal", twoCaptchaCfg(srv.URL))

	bal, err := s.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance() error = %v, want nil", err)
	}
	if bal != 9.99 {
		t.Fatalf("Balance() = %v, want 9.99", bal)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.balanceCalls != 1 {
		t.Fatalf("getbalance calls = %d, want 1", fake.balanceCalls)
	}
}

func TestTwoCaptchaKeyFromEnvWithFallbackWarnings(t *testing.T) {
	fake := &twoCaptchaFake{submitStatus: 1, submitReq: "1", pollResponses: []string{
		`{"status":1,"request":"tok"}`,
	}}
	srv := newTwoCaptchaFake(t, fake)

	t.Run("KeyEnv resolved", func(t *testing.T) {
		t.Setenv("HHAUTO_TEST_2C_KEY", "env-secret")
		cfg := twoCaptchaCfg(srv.URL)
		cfg.Key = ""
		cfg.KeyEnv = "HHAUTO_TEST_2C_KEY"

		s, err := newTwoCaptcha("2c-env", cfg)
		if err != nil {
			t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
		}
		solvedImageToken(t, s, "2c-env")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if got := fake.lastSubmitForm["key"]; got != "env-secret" {
			t.Fatalf("submitted key = %q, want value from KeyEnv", got)
		}
	})

	t.Run("KeyEnv unset falls back to Key with warning", func(t *testing.T) {
		var logBuf syncedBuffer
		s, err := newTwoCaptcha("2c-warn", twoCaptchaCfg(srv.URL))
		if err != nil {
			t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
		}
		s.logger = newTestLogger(&logBuf)
		// Simulate configured-but-unset KeyEnv over an inline key.
		s.keys.keyEnv = "HHAUTO_TEST_MISSING_KEY"

		fake.mu.Lock()
		fake.pollResponses = []string{`{"status":1,"request":"tok"}`}
		fake.mu.Unlock()

		solvedImageToken(t, s, "2c-warn")

		if !strings.Contains(logBuf.String(), "WARN") || !strings.Contains(logBuf.String(), "2c-warn") {
			t.Fatalf("expected WARN mentioning provider id, got: %s", logBuf.String())
		}
		if strings.Contains(logBuf.String(), "test-key") {
			t.Fatalf("log leaks inline key: %s", logBuf.String())
		}
	})

	t.Run("no key at all is a config error", func(t *testing.T) {
		cfg := twoCaptchaCfg(srv.URL)
		cfg.Key = ""
		if _, err := newTwoCaptcha("2c-nokey", cfg); err == nil {
			t.Fatal("newTwoCaptcha() error = nil, want config error for missing key")
		}
	})
}

func TestTwoCaptchaConfigValidation(t *testing.T) {
	fake := &twoCaptchaFake{}
	srv := newTwoCaptchaFake(t, fake)

	cases := []struct {
		name string
		mut  func(*config.CaptchaProvider)
	}{
		{"negative poll", func(c *config.CaptchaProvider) { c.PollMS = -1 }},
		{"negative timeout", func(c *config.CaptchaProvider) { c.TimeoutS = -3 }},
		{"bad base url", func(c *config.CaptchaProvider) { c.BaseURL = "ftp://not-http" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := twoCaptchaCfg(srv.URL)
			tc.mut(&cfg)
			if _, err := newTwoCaptcha("2c-cfg", cfg); err == nil {
				t.Fatalf("newTwoCaptcha() error = nil, want config error")
			}
		})
	}
}

func TestTwoCaptchaUnsupportedChallenge(t *testing.T) {
	fake := &twoCaptchaFake{}
	srv := newTwoCaptchaFake(t, fake)
	s, _ := newTwoCaptcha("2c-u", twoCaptchaCfg(srv.URL))

	if _, err := s.Solve(context.Background(), Challenge{Type: "geetest"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Solve() error = %v, want ErrUnsupported", err)
	}
	if _, err := s.Solve(context.Background(), Challenge{Type: TypeImage}); err == nil {
		t.Fatal("Solve() error = nil for empty image body, want validation error")
	}
	if _, err := s.Solve(context.Background(), Challenge{Type: TypeHCaptcha, SiteKey: "k"}); err == nil {
		t.Fatal("Solve() error = nil for missing pageurl, want validation error")
	}
}

func TestTwoCaptchaHTTPAndJSONErrors(t *testing.T) {
	t.Run("http error status", func(t *testing.T) {
		fake := &twoCaptchaFake{forceHTTPError: http.StatusInternalServerError}
		srv := newTwoCaptchaFake(t, fake)
		s, _ := newTwoCaptcha("2c-500", twoCaptchaCfg(srv.URL))

		if _, err := s.Balance(context.Background()); err == nil {
			t.Fatal("Balance() error = nil, want error for HTTP 500")
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not json</html>"))
		}))
		defer srv.Close()
		s, _ := newTwoCaptcha("2c-json", twoCaptchaCfg(srv.URL))

		if _, err := s.Balance(context.Background()); err == nil {
			t.Fatal("Balance() error = nil, want error for invalid JSON")
		}
	})
	t.Run("unparseable balance", func(t *testing.T) {
		fake := &twoCaptchaFake{balanceResp: "not-a-float"}
		srv := newTwoCaptchaFake(t, fake)
		s, _ := newTwoCaptcha("2c-balfmt", twoCaptchaCfg(srv.URL))

		if _, err := s.Balance(context.Background()); err == nil {
			t.Fatal("Balance() error = nil, want error for unparseable balance")
		}
	})
}
