package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
)

// antiCaptchaFake is an in-process fake of the anti-captcha
// createTask/getTaskResult/getBalance JSON protocol.
type antiCaptchaFake struct {
	mu sync.Mutex

	// createTask reply (marshalled verbatim)
	createReply map[string]any
	// scripted getTaskResult replies in order; after exhaustion the last
	// entry is repeated (use a "processing" reply last for endless polls).
	pollReplies []map[string]any
	balance     float64
	forceStatus int

	createCalls  int
	pollCalls    int
	balanceCalls int

	lastCreate map[string]any
	lastPoll   map[string]any
}

func (f *antiCaptchaFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceStatus != 0 {
		w.WriteHeader(f.forceStatus)
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/createTask":
		f.createCalls++
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.lastCreate = req
		writeMapJSON(w, f.createReply)
	case "/getTaskResult":
		f.pollCalls++
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.lastPoll = req
		if len(f.pollReplies) > 1 {
			writeMapJSON(w, f.pollReplies[0])
			f.pollReplies = f.pollReplies[1:]
			return
		}
		if len(f.pollReplies) == 1 {
			writeMapJSON(w, f.pollReplies[0])
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case "/getBalance":
		f.balanceCalls++
		writeMapJSON(w, map[string]any{"errorId": 0, "balance": f.balance})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeMapJSON(w http.ResponseWriter, m map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(m)
	_, _ = w.Write(data)
}

func newAntiCaptchaFake(t *testing.T, f *antiCaptchaFake) *httptest.Server {
	t.Helper()
	return newTLSCaptchaServer(t, http.HandlerFunc(f.handler))
}

func antiCaptchaCfg(baseURL string) config.CaptchaProvider {
	return config.CaptchaProvider{
		Protocol: "anticaptcha",
		BaseURL:  baseURL,
		Key:      "ac-test-key",
		PollMS:   1,
		TimeoutS: 5,
	}
}

func ready(solution map[string]any) map[string]any {
	return map[string]any{"errorId": 0, "status": "ready", "solution": solution}
}

var processing = map[string]any{"errorId": 0, "status": "processing"}

func TestAntiCaptchaImageSolveSubmitPollText(t *testing.T) {
	fake := &antiCaptchaFake{
		createReply: map[string]any{"errorId": 0, "taskId": 7654321},
		pollReplies: []map[string]any{processing, ready(map[string]any{"text": "deditur"})},
	}
	srv := newAntiCaptchaFake(t, fake)

	s, err := newAntiCaptcha("ac-main", antiCaptchaCfg(srv.URL), nil)
	if err != nil {
		t.Fatalf("newAntiCaptcha() error = %v, want nil", err)
	}

	sol, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	if sol.Token != "deditur" || sol.ProviderID != "ac-main" {
		t.Fatalf("Solve() = %+v, want token deditur from ac-main", sol)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 1 || fake.pollCalls != 2 {
		t.Fatalf("create=%d poll=%d, want 1 and 2", fake.createCalls, fake.pollCalls)
	}
	task, _ := fake.lastCreate["task"].(map[string]any)
	if fake.lastCreate["clientKey"] != "ac-test-key" {
		t.Fatalf("createTask clientKey = %v, want ac-test-key", fake.lastCreate["clientKey"])
	}
	if task["type"] != "ImageToTextTask" || task["body"] != "aGVsbG8=" {
		t.Fatalf("createTask task = %v, want ImageToTextTask with body", task)
	}
	if fake.lastPoll["clientKey"] != "ac-test-key" {
		t.Fatalf("getTaskResult clientKey = %v, want ac-test-key", fake.lastPoll["clientKey"])
	}
	// taskId must round-trip as a number.
	if id, ok := fake.lastPoll["taskId"].(float64); !ok || id != 7654321 {
		t.Fatalf("getTaskResult taskId = %v (%T), want 7654321", fake.lastPoll["taskId"], fake.lastPoll["taskId"])
	}
}

func TestAntiCaptchaHCaptchaAndTurnstileTasks(t *testing.T) {
	cases := []struct {
		name      string
		ch        Challenge
		wantType  string
		solution  map[string]any
		wantToken string
	}{
		{
			name:      "hcaptcha",
			ch:        Challenge{Type: TypeHCaptcha, SiteKey: "hc-key", PageURL: "https://hh.ru/login"},
			wantType:  "HCaptchaTaskProxyless",
			solution:  map[string]any{"gRecaptchaResponse": "P1_hcap_token"},
			wantToken: "P1_hcap_token",
		},
		{
			name:      "turnstile",
			ch:        Challenge{Type: TypeTurnstile, SiteKey: "0xAAAA", PageURL: "https://hh.ru/search"},
			wantType:  "TurnstileTaskProxyless",
			solution:  map[string]any{"token": "0.turn_token"},
			wantToken: "0.turn_token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &antiCaptchaFake{
				createReply: map[string]any{"errorId": 0, "taskId": 100},
				pollReplies: []map[string]any{ready(tc.solution)},
			}
			srv := newAntiCaptchaFake(t, fake)

			s, _ := newAntiCaptcha("ac-"+tc.name, antiCaptchaCfg(srv.URL), nil)

			sol, err := s.Solve(context.Background(), tc.ch)
			if err != nil {
				t.Fatalf("Solve() error = %v, want nil", err)
			}
			if sol.Token != tc.wantToken {
				t.Fatalf("token = %q, want %q", sol.Token, tc.wantToken)
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()
			task, _ := fake.lastCreate["task"].(map[string]any)
			if task["type"] != tc.wantType {
				t.Fatalf("task type = %v, want %s", task["type"], tc.wantType)
			}
			if task["websiteURL"] != tc.ch.PageURL || task["websiteKey"] != tc.ch.SiteKey {
				t.Fatalf("task = %v, want websiteURL/websiteKey from challenge", task)
			}
		})
	}
}

func TestAntiCaptchaCreateTaskProtocolError(t *testing.T) {
	fake := &antiCaptchaFake{createReply: map[string]any{
		"errorId":          1,
		"errorCode":        "ERROR_KEY_DOES_NOT_EXIST",
		"errorDescription": "invalid api key",
	}}
	srv := newAntiCaptchaFake(t, fake)

	s, _ := newAntiCaptcha("ac-bad", antiCaptchaCfg(srv.URL), nil)

	_, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("Solve() error = %v (%T), want *ProtocolError", err, err)
	}
	if pe.Code != "ERROR_KEY_DOES_NOT_EXIST" || pe.ProviderID != "ac-bad" {
		t.Fatalf("ProtocolError = %+v, want code ERROR_KEY_DOES_NOT_EXIST from ac-bad", pe)
	}
	if !strings.Contains(pe.Error(), "invalid api key") {
		t.Fatalf("ProtocolError text = %q, want embedded description", pe.Error())
	}
}

func TestAntiCaptchaPollProtocolError(t *testing.T) {
	fake := &antiCaptchaFake{
		createReply: map[string]any{"errorId": 0, "taskId": 5},
		pollReplies: []map[string]any{{"errorId": 22, "errorCode": "ERROR_CAPTCHA_UNSOLVABLE"}},
	}
	srv := newAntiCaptchaFake(t, fake)

	s, _ := newAntiCaptcha("ac-unsolv", antiCaptchaCfg(srv.URL), nil)

	_, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != "ERROR_CAPTCHA_UNSOLVABLE" {
		t.Fatalf("Solve() error = %v, want ProtocolError(ERROR_CAPTCHA_UNSOLVABLE)", err)
	}
}

func TestAntiCaptchaSolveTimeout(t *testing.T) {
	fake := &antiCaptchaFake{
		createReply: map[string]any{"errorId": 0, "taskId": 6},
		pollReplies: []map[string]any{processing}, // repeated forever
	}
	srv := newAntiCaptchaFake(t, fake)

	s, _ := newAntiCaptcha("ac-slow", antiCaptchaCfg(srv.URL), nil)
	s.pollInterval = time.Millisecond
	s.solveTimeout = 30 * time.Millisecond

	_, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	if err == nil {
		t.Fatal("Solve() error = nil, want timeout error")
	}
	if strings.Contains(err.Error(), "ac-test-key") {
		t.Fatalf("timeout error leaks API key: %v", err)
	}
}

func TestAntiCaptchaBalance(t *testing.T) {
	fake := &antiCaptchaFake{balance: 12.3456}
	srv := newAntiCaptchaFake(t, fake)

	s, _ := newAntiCaptcha("ac-bal", antiCaptchaCfg(srv.URL), nil)

	bal, err := s.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance() error = %v, want nil", err)
	}
	if bal != 12.3456 {
		t.Fatalf("Balance() = %v, want 12.3456", bal)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.balanceCalls != 1 {
		t.Fatalf("getBalance calls = %d, want 1", fake.balanceCalls)
	}
}

func TestAntiCaptchaKeyEnvAndValidation(t *testing.T) {
	fake := &antiCaptchaFake{
		createReply: map[string]any{"errorId": 0, "taskId": 8},
		pollReplies: []map[string]any{ready(map[string]any{"text": "x"})},
	}
	srv := newAntiCaptchaFake(t, fake)

	t.Run("KeyEnv resolved", func(t *testing.T) {
		t.Setenv("HHAUTO_TEST_AC_KEY", "ac-env-secret")
		cfg := antiCaptchaCfg(srv.URL)
		cfg.Key = ""
		cfg.KeyEnv = "HHAUTO_TEST_AC_KEY"

		s, err := newAntiCaptcha("ac-env", cfg, nil)
		if err != nil {
			t.Fatalf("newAntiCaptcha() error = %v, want nil", err)
		}
		if _, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="}); err != nil {
			t.Fatalf("Solve() error = %v, want nil", err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.lastCreate["clientKey"] != "ac-env-secret" {
			t.Fatalf("clientKey = %v, want value from KeyEnv", fake.lastCreate["clientKey"])
		}
	})

	t.Run("no key is config error", func(t *testing.T) {
		cfg := antiCaptchaCfg(srv.URL)
		cfg.Key = ""
		if _, err := newAntiCaptcha("ac-nokey", cfg, nil); err == nil {
			t.Fatal("newAntiCaptcha() error = nil, want config error")
		}
	})

	t.Run("bad base url is config error", func(t *testing.T) {
		cfg := antiCaptchaCfg("javascript:alert(1)")
		if _, err := newAntiCaptcha("ac-badurl", cfg, nil); err == nil {
			t.Fatal("newAntiCaptcha() error = nil, want config error for non-http base_url")
		}
	})

	t.Run("unsupported challenge", func(t *testing.T) {
		s, _ := newAntiCaptcha("ac-u", antiCaptchaCfg(srv.URL), nil)
		if _, err := s.Solve(context.Background(), Challenge{Type: "geetest"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Solve() error = %v, want ErrUnsupported", err)
		}
	})

	t.Run("missing image body", func(t *testing.T) {
		s, _ := newAntiCaptcha("ac-empty", antiCaptchaCfg(srv.URL), nil)
		if _, err := s.Solve(context.Background(), Challenge{Type: TypeImage}); err == nil {
			t.Fatal("Solve() error = nil, want validation error for empty body")
		}
	})
}

func TestAntiCaptchaParallelSolveOnSharedSolver(t *testing.T) {
	fake := &antiCaptchaFake{
		createReply: map[string]any{"errorId": 0, "taskId": 9},
		pollReplies: []map[string]any{ready(map[string]any{"text": "tok"})},
	}
	srv := newAntiCaptchaFake(t, fake)

	var logBuf syncedBuffer
	cfg := antiCaptchaCfg(srv.URL)
	cfg.KeyEnv = "HHAUTO_TEST_PARALLEL_AC" // unset: exercises the inline-key fallback path
	s, err := newAntiCaptcha("ac-parallel", cfg, newTestLogger(&logBuf))
	if err != nil {
		t.Fatalf("newAntiCaptcha() error = %v, want nil", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sol, err := s.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
			if err != nil {
				t.Errorf("Solve() error = %v, want nil", err)
				return
			}
			if sol.Token != "tok" {
				t.Errorf("token = %q, want tok", sol.Token)
			}
		}()
	}
	wg.Wait()
}

func TestAntiCaptchaHTTPSOnlyBaseURL(t *testing.T) {
	s, err := newAntiCaptcha("ac-plain", antiCaptchaCfg("http://api.anticaptcha.example"), nil)
	if err == nil {
		t.Fatal("newAntiCaptcha() error = nil, want config error for http:// base_url (key would travel in cleartext)")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("error = %v, want it to mention https", err)
	}
	if s != nil {
		t.Fatal("solver must be nil on config error")
	}
}

func TestAntiCaptchaHTTPError(t *testing.T) {
	fake := &antiCaptchaFake{forceStatus: http.StatusBadGateway}
	srv := newAntiCaptchaFake(t, fake)

	s, _ := newAntiCaptcha("ac-502", antiCaptchaCfg(srv.URL), nil)
	if _, err := s.Balance(context.Background()); err == nil {
		t.Fatal("Balance() error = nil, want error for HTTP 502")
	}
}
