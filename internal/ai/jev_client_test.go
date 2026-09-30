package ai

// T33: JevClient transport tests — the wire contract (POST, Bearer auth,
// JSON body), the rate-limit/error counters and strict answer-coverage
// validation. The 5s transport timeout is fixed and intentionally not
// exercised here (it would dominate the test runtime).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type capturedRequest struct {
	mu     sync.Mutex
	method string
	path   string
	auth   string
	ct     string
	body   JevRequest
}

func (c *capturedRequest) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method, c.path = r.Method, r.URL.Path
	c.auth = r.Header.Get("Authorization")
	c.ct = r.Header.Get("Content-Type")
	_ = json.NewDecoder(r.Body).Decode(&c.body)
}

func (c *capturedRequest) snapshot() (method, path, auth, ct string, body JevRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.auth, c.ct, c.body
}

func TestJevClientWireContract(t *testing.T) {
	capt := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
		_ = json.NewEncoder(w).Encode(JevResponse{Answers: []JevAnswer{{
			ID: "q1", Value: json.RawMessage(`"a"`), Probability: 0.94, Confidence: 0.91,
		}}})
	}))
	defer srv.Close()

	c := NewJevClient(srv.URL+"/", "secret-key", JevClientOptions{}) // trailing slash is trimmed
	resp, err := c.Solve(context.Background(), JevRequest{
		State:     `{"text":"hi"}`,
		Questions: []JevQuestion{{ID: "q1", Type: JevQuestionChoice, Prompt: "pick", Options: []string{"a", "b"}}},
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if len(resp.Answers) != 1 || resp.Answers[0].ID != "q1" || resp.Answers[0].Confidence != 0.91 {
		t.Errorf("response = %+v", resp)
	}
	method, path, auth, ct, body := capt.snapshot()
	if method != http.MethodPost || path != "/" {
		t.Errorf("request = %s %s, want POST /", method, path)
	}
	if auth != "Bearer secret-key" {
		t.Errorf("authorization = %q, want bearer form", auth)
	}
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	if body.State != `{"text":"hi"}` {
		t.Errorf("state = %q", body.State)
	}
	if len(body.Questions) != 1 {
		t.Fatalf("questions = %+v", body.Questions)
	}
	q := body.Questions[0]
	if q.ID != "q1" || q.Type != JevQuestionChoice || q.Prompt != "pick" || len(q.Options) != 2 {
		t.Errorf("question = %+v", q)
	}
	if st := c.Stats(); st.Requests != 1 || st.Errors != 0 || st.RateLimited != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestJevClientRateLimitCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewJevClient(srv.URL, "k", JevClientOptions{})
	_, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want HTTP 429", err)
	}
	st := c.Stats()
	if st.Requests != 1 || st.RateLimited != 1 || st.Errors != 1 {
		t.Errorf("stats = %+v, want request/rate-limited/error all 1", st)
	}
}

func TestJevClientMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewJevClient(srv.URL, "k", JevClientOptions{})
	_, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode failure", err)
	}
}

func TestJevClientAnswerCoverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers func(qs []JevQuestion) []JevAnswer
		want    string
	}{
		{
			name: "missing answer",
			answers: func(qs []JevQuestion) []JevAnswer {
				return []JevAnswer{{ID: qs[0].ID, Value: json.RawMessage(`1`)}}
			},
			want: "no answer",
		},
		{
			name: "duplicate answers",
			answers: func(qs []JevQuestion) []JevAnswer {
				a := JevAnswer{ID: qs[0].ID, Value: json.RawMessage(`1`)}
				return []JevAnswer{a, a}
			},
			want: "duplicate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req JevRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				_ = json.NewEncoder(w).Encode(JevResponse{Answers: tc.answers(req.Questions)})
			}))
			defer srv.Close()

			c := NewJevClient(srv.URL, "k", JevClientOptions{})
			_, err := c.Solve(context.Background(), JevRequest{
				State: "s",
				Questions: []JevQuestion{
					{ID: "a", Type: JevQuestionNoul},
					{ID: "b", Type: JevQuestionNoul},
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestJevClientUnreachableEndpoint(t *testing.T) {
	c := NewJevClient("http://127.0.0.1:1/v1/systemone", "k", JevClientOptions{})
	_, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want unreachable endpoint", err)
	}
}

// T74: the openai-chat wire (OpenRouter-compatible chat/completions
// envelope). All tests run against fake httptest servers — no real keys.

func TestJevClientOpenAIChatWire(t *testing.T) {
	var (
		mu      sync.Mutex
		auth    string
		referer string
		title   string
		chatReq openaiChatRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
		referer = r.Header.Get("HTTP-Referer")
		title = r.Header.Get("X-OpenRouter-Title")
		_ = json.NewDecoder(r.Body).Decode(&chatReq)
		content := `{"answers":[{"id":"q1","value":"a","probability":0.94,"confidence":0.91}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}}},
		})
	}))
	defer srv.Close()

	c := NewJevClient(srv.URL, "or-key", JevClientOptions{
		Wire: "openai-chat", Model: "~typesafe/jev-latest",
		HTTPReferer: "https://example.com/", XTitle: "hhauto",
	})
	resp, err := c.Solve(context.Background(), JevRequest{
		State:     `{"text":"hi"}`,
		Questions: []JevQuestion{{ID: "q1", Type: JevQuestionChoice, Prompt: "pick", Options: []string{"a", "b"}}},
	})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if len(resp.Answers) != 1 || resp.Answers[0].ID != "q1" || resp.Answers[0].Confidence != 0.91 {
		t.Errorf("response = %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer or-key" {
		t.Errorf("authorization = %q", auth)
	}
	if referer != "https://example.com/" || title != "hhauto" {
		t.Errorf("attribution headers = %q / %q", referer, title)
	}
	if chatReq.Model != "~typesafe/jev-latest" {
		t.Errorf("chat model = %q", chatReq.Model)
	}
	if len(chatReq.Messages) != 2 || chatReq.Messages[0].Role != "system" || chatReq.Messages[1].Role != "user" {
		t.Fatalf("chat messages = %+v", chatReq.Messages)
	}
	var user JevRequest
	if err := json.Unmarshal([]byte(chatReq.Messages[1].Content), &user); err != nil {
		t.Fatalf("user content is not the state+questions JSON: %v", err)
	}
	if user.State != `{"text":"hi"}` || len(user.Questions) != 1 || user.Questions[0].ID != "q1" {
		t.Errorf("user content = %+v", user)
	}
	if user.Model != "" {
		t.Errorf("user content carries model %q", user.Model)
	}
	if st := c.Stats(); st.Requests != 1 || st.Errors != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestJevClientOpenAIChatFencedAndProseContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := "Sure!\n```json\n{\"answers\":[{\"id\":\"q\",\"value\":true,\"probability\":0.9,\"confidence\":0.9}]}\n```"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer srv.Close()

	c := NewJevClient(srv.URL, "k", JevClientOptions{Wire: "openai-chat", Model: "m"})
	resp, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q", Type: JevQuestionNoul}}})
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	var v bool
	if err := json.Unmarshal(resp.Answers[0].Value, &v); err != nil || !v {
		t.Errorf("answers[0].value = %s (err %v)", resp.Answers[0].Value, err)
	}
}

func TestJevClientOpenAIChatMalformedReplies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"empty content", "", "no content"},
		{"prose only", "I cannot answer that.", "not valid answers JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": tc.content}}},
				})
			}))
			defer srv.Close()

			c := NewJevClient(srv.URL, "k", JevClientOptions{Wire: "openai-chat", Model: "m"})
			_, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if st := c.Stats(); st.Errors != 1 {
				t.Errorf("stats = %+v, want one error", st)
			}
		})
	}
}

func TestJevClientOpenAIChatNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	c := NewJevClient(srv.URL, "k", JevClientOptions{Wire: "openai-chat", Model: "m"})
	_, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}})
	if err == nil || !strings.Contains(err.Error(), "no content") {
		t.Fatalf("err = %v, want empty choices failure", err)
	}
}

func TestJevClientSystemOneModelField(t *testing.T) {
	var bodies []JevRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JevRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		bodies = append(bodies, req)
		_ = json.NewEncoder(w).Encode(JevResponse{Answers: []JevAnswer{{
			ID: "q", Value: json.RawMessage(`true`), Probability: 1, Confidence: 1,
		}}})
	}))
	defer srv.Close()

	// Empty model (default): the T33 shape — no model field in the body.
	c := NewJevClient(srv.URL, "k", JevClientOptions{})
	if _, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}}); err != nil {
		t.Fatalf("Solve default: %v", err)
	}
	if bodies[0].Model != "" {
		t.Errorf("default body carries model %q", bodies[0].Model)
	}

	// Configured model: the field rides in the systemone body.
	c = NewJevClient(srv.URL, "k", JevClientOptions{Model: "jev-1"})
	if _, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}}); err != nil {
		t.Fatalf("Solve with model: %v", err)
	}
	if bodies[1].Model != "jev-1" {
		t.Errorf("body model = %q, want jev-1", bodies[1].Model)
	}
}
