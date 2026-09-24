package ai

import (
	"context"
	"net/http"
	"testing"
)

func testResponsesProvider(t *testing.T, base string, opts ...Option) *OpenAIResponsesProvider {
	t.Helper()
	t.Setenv("HHAUTO_TEST_AI_KEY", "k-test")
	p, err := NewOpenAIResponsesProvider("primary", testProviderCfg(base), opts...)
	if err != nil {
		t.Fatalf("NewOpenAIResponsesProvider: %v", err)
	}
	return p
}

const testValidResponsesReply = `{
  "id": "resp_1",
  "model": "o5-mini",
  "output": [
    {"type": "reasoning", "summary": []},
    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "{\"ok\":true}"}]}
  ],
  "usage": {"input_tokens": 11, "output_tokens": 7}
}`

func TestOpenAIResponsesValidReplyAndRequestShape(t *testing.T) {
	f := newFakeServer(t, func(call int, r *http.Request, raw []byte) (int, map[string]string, string) {
		return 200, nil, testValidResponsesReply
	})
	p := testResponsesProvider(t, f.srv.URL)
	bound, err := NewBoundProvider(p, "o5-mini")
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
	if resp.TokensIn != 11 || resp.TokensOut != 7 {
		t.Errorf("tokens = %d/%d", resp.TokensIn, resp.TokensOut)
	}
	if resp.Model != "o5-mini" {
		t.Errorf("model = %q", resp.Model)
	}
	body := f.lastBody()
	if body["model"] != "o5-mini" {
		t.Errorf("request model = %v", body["model"])
	}
	if body["instructions"] != "SYS" {
		t.Errorf("instructions = %v, want system prompt at top level", body["instructions"])
	}
	if body["max_output_tokens"].(float64) != 900 {
		t.Errorf("max_output_tokens = %v", body["max_output_tokens"])
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Errorf("reasoning.effort = %v", reasoning["effort"])
	}
	text := body["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true || format["name"] != "vacancy_scoring" {
		t.Errorf("text.format = %v", format)
	}
	if _, ok := format["schema"].(map[string]any); !ok {
		t.Error("text.format.schema missing")
	}
	input := body["input"].([]any)
	first := input[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("input[0].role = %v", first["role"])
	}
	parts := first["content"].([]any)
	part := parts[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "USER DATA" {
		t.Errorf("input text part = %v", part)
	}
	if p.Dialect() != DialectOpenAIResponses {
		t.Errorf("dialect = %q", p.Dialect())
	}
}

func TestOpenAIResponsesNoMessageItemFails(t *testing.T) {
	f := newFakeServer(t, func(call int, r *http.Request, raw []byte) (int, map[string]string, string) {
		return 200, nil, `{"id":"r","model":"m","output":[{"type":"reasoning","summary":[]}],"usage":{}}`
	})
	p := testResponsesProvider(t, f.srv.URL)
	bound, _ := NewBoundProvider(p, "m")
	if _, err := bound.Complete(context.Background(), chatSchemaRequest()); err == nil {
		t.Fatal("expected error when output carries no assistant message")
	}
}

func TestOpenAIResponsesMultipleTextPartsConcatenated(t *testing.T) {
	f := newFakeServer(t, func(call int, r *http.Request, raw []byte) (int, map[string]string, string) {
		return 200, nil, `{"id":"r","model":"m","output":[{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"{\"a\":"},{"type":"output_text","text":"1}"}
		]}],"usage":{"input_tokens":1,"output_tokens":2}}`
	})
	p := testResponsesProvider(t, f.srv.URL)
	bound, _ := NewBoundProvider(p, "m")
	resp, err := bound.Complete(context.Background(), chatSchemaRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != `{"a":1}` {
		t.Errorf("content = %q", resp.Content)
	}
}
