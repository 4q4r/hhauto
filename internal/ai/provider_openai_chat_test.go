package ai

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

// --- shared helpers -------------------------------------------------------

type recordedRequest struct {
	body   map[string]any
	raw    []byte
	header http.Header
	at     time.Time
}

type fakeServer struct {
	mu   sync.Mutex
	srv  *httptest.Server
	reqs []recordedRequest
	// respond consumes one scripted reply per call; the last one repeats.
	respond func(call int, r *http.Request, raw []byte) (status int, headers map[string]string, body string)
}

func newFakeServer(t *testing.T, respond func(call int, r *http.Request, raw []byte) (int, map[string]string, string)) *fakeServer {
	t.Helper()
	f := &fakeServer{respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		call := len(f.reqs) + 1
		f.reqs = append(f.reqs, recordedRequest{body: body, raw: raw, header: r.Header.Clone(), at: time.Now()})
		f.mu.Unlock()
		status, headers, respBody := respond(call, r, raw)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeServer) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1].body
}

func (f *fakeServer) bodyOf(call int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[call-1].body
}

func (f *fakeServer) headerOf(call int) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[call-1].header
}

type sleepRecorder struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (s *sleepRecorder) fn(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.durations = append(s.durations, d)
	s.mu.Unlock()
	return nil
}

func (s *sleepRecorder) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.durations...)
}

func testProviderCfg(baseURL string) config.AIProvider {
	return config.AIProvider{
		Dialect:   "",
		BaseURL:   baseURL,
		APIKeyEnv: "HHAUTO_TEST_AI_KEY",
		TimeoutS:  5,
	}
}

const testValidChatReply = `{"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`

func chatSchemaRequest() Request {
	return Request{
		Task:            TaskVacancyScoring,
		System:          "SYS",
		Messages:        []Message{{Role: "user", Content: "USER DATA"}},
		JSONSchema:      json.RawMessage(`{"type":"object"}`),
		MaxOutputTokens: 900,
		Temperature:     0.3,
		Effort:          "low",
	}
}

func testChatProvider(t *testing.T, base string, opts ...Option) *OpenAIChatProvider {
	t.Helper()
	t.Setenv("HHAUTO_TEST_AI_KEY", "k-test")
	p, err := NewOpenAIChatProvider("primary", testProviderCfg(base), opts...)
	if err != nil {
		t.Fatalf("NewOpenAIChatProvider: %v", err)
	}
	return p
}

// --- openai-chat ----------------------------------------------------------

func TestOpenAIChatValidReplyAndRequestShape(t *testing.T) {
	f := newFakeServer(t, func(_ int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		return http.StatusOK, nil, testValidChatReply
	})
	p := testChatProvider(t, f.srv.URL)
	bound, err := NewBoundProvider(p, "gpt-test-model")
	if err != nil {
		t.Fatalf("NewBoundProvider: %v", err)
	}
	resp, err := bound.Complete(context.Background(), chatSchemaRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != `{"ok":true}` {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.TokensIn != 12 || resp.TokensOut != 34 {
		t.Errorf("tokens = %d/%d", resp.TokensIn, resp.TokensOut)
	}
	if resp.Model != "gpt-test-model" {
		t.Errorf("model = %q", resp.Model)
	}
}

func TestOpenAIChatCompleteModelBindsModelAndParams(t *testing.T) {
	f := newFakeServer(t, func(_ int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		return http.StatusOK, nil, testValidChatReply
	})
	p := testChatProvider(t, f.srv.URL)
	bound, err := NewBoundProvider(p, "gpt-test-model")
	if err != nil {
		t.Fatalf("NewBoundProvider: %v", err)
	}
	resp, err := bound.Complete(context.Background(), chatSchemaRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "gpt-test-model" {
		t.Errorf("resp.Model = %q", resp.Model)
	}
	body := f.lastBody()
	if body["model"] != "gpt-test-model" {
		t.Errorf("request model = %v", body["model"])
	}
	if body["max_completion_tokens"].(float64) != 900 {
		t.Errorf("max_completion_tokens = %v", body["max_completion_tokens"])
	}
	if _, hasMaxTokens := body["max_tokens"]; hasMaxTokens {
		t.Error("deprecated max_tokens must not be sent")
	}
	if body["temperature"].(float64) != 0.3 {
		t.Errorf("temperature = %v", body["temperature"])
	}
	if body["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v", body["reasoning_effort"])
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing: %v", body)
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v", rf["type"])
	}
	js := rf["json_schema"].(map[string]any)
	if js["name"] != "vacancy_scoring" {
		t.Errorf("json_schema.name = %v", js["name"])
	}
	if js["strict"] != true {
		t.Errorf("json_schema.strict = %v", js["strict"])
	}
	if js["schema"].(map[string]any) == nil {
		t.Error("json_schema.schema missing")
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "SYS" {
		t.Errorf("system message = %v", first)
	}
	if !strings.Contains(f.headerOf(1).Get("Authorization"), "k-test") {
		t.Error("missing bearer token")
	}
	if p.Dialect() != DialectOpenAIChat {
		t.Errorf("dialect = %q", p.Dialect())
	}
}

func TestOpenAIChatTemperatureUnsupportedDropsParamAndRemembers(t *testing.T) {
	f := newFakeServer(t, func(_ int, _ *http.Request, raw []byte) (int, map[string]string, string) {
		if strings.Contains(string(raw), `"temperature"`) {
			return http.StatusBadRequest, nil, `{"error":{"message":"temperature is not supported with this model","type":"invalid_request_error","param":"temperature","code":"unsupported_parameter"}}`
		}
		return http.StatusOK, nil, testValidChatReply
	})
	profiles := NewModelProfiles()
	p := testChatProvider(t, f.srv.URL, WithProfiles(profiles))
	bound, _ := NewBoundProvider(p, "gpt-test-model")
	if _, err := bound.Complete(context.Background(), chatSchemaRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.calls() != 2 {
		t.Fatalf("calls = %d, want 2 (retry without temperature)", f.calls())
	}
	if _, has := f.bodyOf(2)["temperature"]; has {
		t.Error("retry still carries temperature")
	}
	// Profile memory: next call must not attempt temperature at all.
	if _, err := bound.Complete(context.Background(), chatSchemaRequest()); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if f.calls() != 3 {
		t.Fatalf("calls after memory = %d, want 3", f.calls())
	}
	if _, has := f.bodyOf(3)["temperature"]; has {
		t.Error("profile memory did not suppress temperature")
	}
	if !profiles.TemperatureUnsupported("gpt-test-model") {
		t.Error("model profile not recorded")
	}
}

func TestOpenAIChat429HonorsRetryAfter(t *testing.T) {
	f := newFakeServer(t, func(call int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		if call == 1 {
			return http.StatusTooManyRequests, map[string]string{"Retry-After": "2"}, `{"error":{"message":"rate limited"}}`
		}
		return http.StatusOK, nil, testValidChatReply
	})
	sleeps := &sleepRecorder{}
	p := testChatProvider(t, f.srv.URL, WithSleep(sleeps.fn))
	bound, _ := NewBoundProvider(p, "m")
	if _, err := bound.Complete(context.Background(), chatSchemaRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.calls() != 2 {
		t.Fatalf("calls = %d", f.calls())
	}
	durs := sleeps.all()
	if len(durs) != 1 || durs[0] < 2*time.Second {
		t.Errorf("sleeps = %v, want [>=2s]", durs)
	}
}

func TestOpenAIChat5xxBackoffFullJitter(t *testing.T) {
	f := newFakeServer(t, func(call int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		if call < 3 {
			return http.StatusInternalServerError, nil, `{"error":{"message":"boom"}}`
		}
		return http.StatusOK, nil, testValidChatReply
	})
	sleeps := &sleepRecorder{}
	p := testChatProvider(t, f.srv.URL, WithSleep(sleeps.fn))
	bound, _ := NewBoundProvider(p, "m")
	if _, err := bound.Complete(context.Background(), chatSchemaRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.calls() != 3 {
		t.Fatalf("calls = %d", f.calls())
	}
	durs := sleeps.all()
	// Full jitter: uniform in [0, 1s) then [0, 2s).
	if len(durs) != 2 {
		t.Fatalf("sleeps = %v, want 2", durs)
	}
	if durs[0] < 0 || durs[0] >= time.Second {
		t.Errorf("sleep1 = %v, want in [0, 1s)", durs[0])
	}
	if durs[1] < 0 || durs[1] >= 2*time.Second {
		t.Errorf("sleep2 = %v, want in [0, 2s)", durs[1])
	}
}

func TestOpenAIChatExhaustedRetriesReturnsAPIError(t *testing.T) {
	f := newFakeServer(t, func(_ int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		return http.StatusBadGateway, nil, `{"error":{"message":"upstream"}}`
	})
	sleeps := &sleepRecorder{}
	p := testChatProvider(t, f.srv.URL, WithSleep(sleeps.fn))
	bound, _ := NewBoundProvider(p, "m")
	_, err := bound.Complete(context.Background(), chatSchemaRequest())
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("status = %d", apiErr.Status)
	}
	if f.calls() != 4 {
		t.Errorf("calls = %d, want 4 (1 + 3 retries)", f.calls())
	}
}

func TestOpenAIChatMissingAPIKeyFails(t *testing.T) {
	p, err := NewOpenAIChatProvider("x", config.AIProvider{BaseURL: "http://127.0.0.1:1", APIKeyEnv: "HHAUTO_UNSET_KEY_ENV"})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := p.Complete(context.Background(), chatSchemaRequest()); err == nil {
		t.Fatal("expected error for missing API key")
	}
}
