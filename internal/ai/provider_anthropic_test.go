package ai

import (
	"context"
	"net/http"
	"testing"
)

func testAnthropicProvider(t *testing.T, base string, opts ...Option) *AnthropicProvider {
	t.Helper()
	t.Setenv("HHAUTO_TEST_AI_KEY", "k-ant")
	p, err := NewAnthropicProvider("primary", testProviderCfg(base), opts...)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	return p
}

const testValidAnthropicReply = `{
  "model": "claude-test",
  "content": [{"type": "text", "text": "{\"ok\":true}"}],
  "usage": {"input_tokens": 5, "output_tokens": 3},
  "stop_reason": "end_turn"
}`

func TestAnthropicValidReplyAndRequestShape(t *testing.T) {
	f := newFakeServer(t, func(_ int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		return 200, nil, testValidAnthropicReply
	})
	p := testAnthropicProvider(t, f.srv.URL)
	bound, err := NewBoundProvider(p, "claude-test")
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
	if resp.TokensIn != 5 || resp.TokensOut != 3 {
		t.Errorf("tokens = %d/%d", resp.TokensIn, resp.TokensOut)
	}

	body := f.lastBody()
	if body["model"] != "claude-test" {
		t.Errorf("request model = %v", body["model"])
	}
	// max_tokens is REQUIRED by the Messages API.
	if body["max_tokens"].(float64) != 900 {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	// system is a TOP-LEVEL field, never a message role. It carries the
	// trusted prompt plus the duplicated schema (dialect contract).
	sysTop, _ := body["system"].(string)
	if !contains(sysTop, "SYS") {
		t.Errorf("system = %q, want the trusted prompt at top level", body["system"])
	}
	msgs := body["messages"].([]any)
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			t.Errorf("system role leaked into messages: %v", mm)
		}
	}
	if len(msgs) != 1 {
		t.Errorf("messages = %v", msgs)
	}
	// Structured output: schema duplicated into the system text (dialect rule).
	sys, _ := body["system"].(string)
	if !contains(sys, `{"type":"object"}`) {
		t.Errorf("schema not duplicated into system text: %q", sys)
	}
	if !contains(sys, "SYS") {
		t.Errorf("original system text lost: %q", sys)
	}
	h := f.headerOf(1)
	if h.Get("x-api-key") != "k-ant" {
		t.Errorf("x-api-key = %q", h.Get("x-api-key"))
	}
	if h.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("anthropic-version = %q", h.Get("anthropic-version"))
	}
	if p.Dialect() != DialectAnthropicMessages {
		t.Errorf("dialect = %q", p.Dialect())
	}
	// Effort "low" on this request: adaptive thinking + output_config.effort.
	if _, has := body["thinking"]; !has {
		t.Error("thinking params missing for effort=low")
	}
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking.type = %v", thinking["type"])
	}
	if body["output_config"].(map[string]any)["effort"] != "low" {
		t.Errorf("output_config.effort = %v", body["output_config"])
	}
}

func TestAnthropicMaxTokensDefaultsWhenUnset(t *testing.T) {
	f := newFakeServer(t, func(_ int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		return 200, nil, testValidAnthropicReply
	})
	p := testAnthropicProvider(t, f.srv.URL)
	bound, _ := NewBoundProvider(p, "claude-test")
	req := chatSchemaRequest()
	req.MaxOutputTokens = 0
	req.Effort = EffortNone
	if _, err := bound.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := f.lastBody()
	if body["max_tokens"].(float64) != defaultAnthropicMaxTokens {
		t.Errorf("max_tokens default = %v", body["max_tokens"])
	}
	if _, has := body["thinking"]; has {
		t.Error("thinking must be absent when effort=none")
	}
	if _, has := body["output_config"]; has {
		t.Error("output_config must be absent when effort=none")
	}
}

func TestAnthropicEffortMapping(t *testing.T) {
	cases := map[string]any{"minimal": "low", "xhigh": "xhigh", "max": "max", "high": "high"}
	for effort, want := range cases {
		t.Run(effort, func(t *testing.T) {
			f := newFakeServer(t, func(_ int, _ *http.Request, _ []byte) (int, map[string]string, string) {
				return 200, nil, testValidAnthropicReply
			})
			p := testAnthropicProvider(t, f.srv.URL)
			bound, _ := NewBoundProvider(p, "claude-test")
			req := chatSchemaRequest()
			req.Effort = effort
			if _, err := bound.Complete(context.Background(), req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			got := f.lastBody()["output_config"].(map[string]any)["effort"]
			if got != want {
				t.Errorf("effort %q mapped to %v, want %v", effort, got, want)
			}
		})
	}
}

func TestAnthropicThinkingUnsupportedRetriesWithoutIt(t *testing.T) {
	f := newFakeServer(t, func(call int, _ *http.Request, _ []byte) (int, map[string]string, string) {
		if call == 1 {
			return 400, nil, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.type.enabled is not supported by this model"}}`
		}
		return 200, nil, testValidAnthropicReply
	})
	p := testAnthropicProvider(t, f.srv.URL)
	bound, _ := NewBoundProvider(p, "claude-test")
	if _, err := bound.Complete(context.Background(), chatSchemaRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.calls())
	}
	body := f.bodyOf(2)
	if _, has := body["thinking"]; has {
		t.Error("retry still carries thinking")
	}
	if _, has := body["output_config"]; has {
		t.Error("retry still carries output_config")
	}
	if body["max_tokens"] == nil {
		t.Error("max_tokens lost on retry")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
