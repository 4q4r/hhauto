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

	c := NewJevClient(srv.URL+"/", "secret-key") // trailing slash is trimmed
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

	c := NewJevClient(srv.URL, "k")
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

	c := NewJevClient(srv.URL, "k")
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

			c := NewJevClient(srv.URL, "k")
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
	c := NewJevClient("http://127.0.0.1:1/v1/systemone", "k")
	_, err := c.Solve(context.Background(), JevRequest{State: "s", Questions: []JevQuestion{{ID: "q"}}})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want unreachable endpoint", err)
	}
}
