package captcha

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
)

func TestResolveBuildsChainInConfiguredOrder(t *testing.T) {
	twoFake := &twoCaptchaFake{submitStatus: 1, submitReq: "11", pollResponses: []string{
		`{"status":1,"request":"two-token"}`,
	}}
	twoSrv := newTwoCaptchaFake(t, twoFake)
	acFake := &antiCaptchaFake{
		createReply: map[string]any{"errorId": 0, "taskId": 12},
		pollReplies: []map[string]any{ready(map[string]any{"text": "ac-token"})},
	}
	acSrv := newAntiCaptchaFake(t, acFake)

	resolver, err := New(map[string]config.CaptchaProvider{
		"2c-main": {
			Protocol: "2captcha",
			BaseURL:  twoSrv.URL,
			Key:      "k2c",
			PollMS:   1,
			TimeoutS: 5,
		},
		"ac-main": {
			Protocol: "anticaptcha",
			BaseURL:  acSrv.URL,
			Key:      "kac",
			PollMS:   1,
			TimeoutS: 5,
		},
	}, func(accountID string) []string {
		if accountID != "acct-1" {
			t.Fatalf("chainFor called with %q, want acct-1", accountID)
		}
		return []string{"2c-main", "ac-main"}
	}, nil)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	chain, err := resolver.ChainFor("acct-1")
	if err != nil {
		t.Fatalf("ChainFor() error = %v, want nil", err)
	}

	sol, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	// First provider in the chain must win.
	if sol.Token != "two-token" || sol.ProviderID != "2c-main" {
		t.Fatalf("Solve() = %+v, want two-token from 2c-main (chain order broken)", sol)
	}
}

func TestResolveUnknownProviderInChain(t *testing.T) {
	twoFake := &twoCaptchaFake{}
	srv := newTwoCaptchaFake(t, twoFake)

	resolver, err := New(map[string]config.CaptchaProvider{
		"2c-main": {Protocol: "2captcha", BaseURL: srv.URL, Key: "k", PollMS: 1, TimeoutS: 5},
	}, func(string) []string { return []string{"2c-main", "ghost"} }, nil)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	_, err = resolver.ChainFor("acct-1")
	if err == nil {
		t.Fatal("ChainFor() error = nil, want config error for unknown provider")
	}
	if !errors.Is(err, ErrNoProviders) && !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("ChainFor() error = %v, want it to name the unknown provider", err)
	}
}

func TestResolveConfigErrors(t *testing.T) {
	t.Run("unknown protocol", func(t *testing.T) {
		_, err := New(map[string]config.CaptchaProvider{
			"weird": {Protocol: "deathbycaptcha", Key: "k"},
		}, func(string) []string { return nil }, nil)
		if err == nil || !strings.Contains(err.Error(), "protocol") {
			t.Fatalf("New() error = %v, want unknown-protocol config error", err)
		}
	})

	t.Run("empty provider map", func(t *testing.T) {
		_, err := New(nil, func(string) []string { return nil }, nil)
		if !errors.Is(err, ErrNoProviders) {
			t.Fatalf("New() error = %v, want ErrNoProviders", err)
		}
	})

	t.Run("nil chainFor", func(t *testing.T) {
		_, err := New(map[string]config.CaptchaProvider{
			"2c-main": {Protocol: "2captcha", Key: "k"},
		}, nil, nil)
		if err == nil {
			t.Fatal("New() error = nil, want config error for nil chainFor")
		}
	})

	t.Run("empty account chain", func(t *testing.T) {
		resolver, err := New(map[string]config.CaptchaProvider{
			"2c-main": {Protocol: "2captcha", Key: "k", PollMS: 1, TimeoutS: 5},
		}, func(string) []string { return nil }, nil)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		_, err = resolver.ChainFor("acct-2")
		if !errors.Is(err, ErrNoProviders) {
			t.Fatalf("ChainFor() error = %v, want ErrNoProviders for empty chain", err)
		}
	})

	t.Run("invalid provider config surfaces from New", func(t *testing.T) {
		_, err := New(map[string]config.CaptchaProvider{
			"2c-main": {Protocol: "2captcha", PollMS: -5, Key: "k"},
		}, func(string) []string { return nil }, nil)
		if err == nil {
			t.Fatal("New() error = nil, want config error for negative poll_interval_ms")
		}
	})
}

func TestNewInjectsLoggerIntoSolvers(t *testing.T) {
	// Finding 4: solvers must log through the application logger passed to
	// New, not the slog default — the app wraps it with secret redaction.
	var logBuf syncedBuffer
	inlineProvider := map[string]config.CaptchaProvider{
		"2c-inline": {Protocol: "2captcha", Key: "inline-secret", PollMS: 1, TimeoutS: 5},
	}

	_, err := New(inlineProvider, func(string) []string { return []string{"2c-inline"} }, newTestLogger(&logBuf))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if !strings.Contains(logBuf.String(), "WARN") || !strings.Contains(logBuf.String(), "2c-inline") {
		t.Fatalf("expected inline-key WARN in the injected logger, got: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "inline-secret") {
		t.Fatalf("injected logger output leaks the key: %s", logBuf.String())
	}

	// nil logger is allowed for compatibility and must not panic.
	if _, err := New(inlineProvider, func(string) []string { return []string{"2c-inline"} }, nil); err != nil {
		t.Fatalf("New() with nil logger error = %v, want nil", err)
	}
}

func TestNewRejectsPlaintextHTTPBaseURL(t *testing.T) {
	_, err := New(map[string]config.CaptchaProvider{
		"2c-http": {Protocol: "2captcha", BaseURL: "http://2captcha.example", Key: "k"},
	}, func(string) []string { return []string{"2c-http"} }, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("New() error = %v, want https-only config error", err)
	}

	_, err = New(map[string]config.CaptchaProvider{
		"ac-http": {Protocol: "anticaptcha", BaseURL: "http://api.anticaptcha.example", Key: "k"},
	}, func(string) []string { return []string{"ac-http"} }, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("New() error = %v, want https-only config error", err)
	}
}

func TestResolveKeyEnvPlumbedThroughChain(t *testing.T) {
	fake := &twoCaptchaFake{submitStatus: 1, submitReq: "21", pollResponses: []string{
		`{"status":1,"request":"env-token"}`,
	}}
	srv := newTwoCaptchaFake(t, fake)
	t.Setenv("HHAUTO_TEST_RESOLVE_KEY", "resolved-secret")

	resolver, err := New(map[string]config.CaptchaProvider{
		"2c-env": {Protocol: "2captcha", BaseURL: srv.URL, KeyEnv: "HHAUTO_TEST_RESOLVE_KEY", PollMS: 1, TimeoutS: 5},
	}, func(string) []string { return []string{"2c-env"} }, nil)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	chain, err := resolver.ChainFor("acct-1")
	if err != nil {
		t.Fatalf("ChainFor() error = %v, want nil", err)
	}
	sol, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "aGVsbG8="})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	if sol.Token != "env-token" {
		t.Fatalf("token = %q, want env-token", sol.Token)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.lastSubmitForm["key"]; got != "resolved-secret" {
		t.Fatalf("submitted key = %q, want env value", got)
	}
}

func TestResolveDefaultBaseURLsAndTiming(t *testing.T) {
	// Providers without base_url target the canonical service endpoints and
	// zero PollMS/TimeoutS yield the protocol defaults — asserted without
	// any network access.
	two, err := newTwoCaptcha("2c-def", config.CaptchaProvider{Protocol: "2captcha", Key: "k"}, nil)
	if err != nil {
		t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
	}
	if two.baseURL != "https://2captcha.com" {
		t.Fatalf("2captcha default baseURL = %q, want https://2captcha.com", two.baseURL)
	}
	if two.pollInterval != 5000*time.Millisecond || two.solveTimeout != 120*time.Second {
		t.Fatalf("2captcha defaults poll=%v timeout=%v, want 5s/120s", two.pollInterval, two.solveTimeout)
	}

	ac, err := newAntiCaptcha("ac-def", config.CaptchaProvider{Protocol: "anticaptcha", Key: "k"}, nil)
	if err != nil {
		t.Fatalf("newAntiCaptcha() error = %v, want nil", err)
	}
	if ac.baseURL != "https://api.anti-captcha.com" {
		t.Fatalf("anticaptcha default baseURL = %q, want https://api.anti-captcha.com", ac.baseURL)
	}

	// base_url is what makes RuCaptcha/SolveCaptcha or CapMonster Cloud work:
	// same protocol, different host.
	cloned, err := newTwoCaptcha("ru", config.CaptchaProvider{Protocol: "2captcha", BaseURL: "https://api.rucaptcha.com/", Key: "k"}, nil)
	if err != nil {
		t.Fatalf("newTwoCaptcha() error = %v, want nil", err)
	}
	if cloned.baseURL != "https://api.rucaptcha.com" {
		t.Fatalf("clone baseURL = %q, want trailing slash trimmed", cloned.baseURL)
	}
}
