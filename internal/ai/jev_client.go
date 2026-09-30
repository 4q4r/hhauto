package ai

// JevClient is the transport for the optional Jev (System One) prefilter
// (T33): one POST to the configured Jev-compatible endpoint, Bearer auth, a
// fixed short timeout and typed request/response structs.
//
// Wire contract (T33 owner-specified live contract, implemented verbatim):
//
//	POST {endpoint}                       # [ai.jev].endpoint (default https://api.typesafe.ai/v1/systemone)
//	Authorization: Bearer <key>           # key from [ai.jev].api_key or .api_key_env (T74)
//	Content-Type: application/json
//	{"state": "<compact JSON>", "questions": [{"id", "type": "noul"|"choice"|"score", "prompt", "options"?}]}
//	→ {"answers": [{"id", "value", "probability", "confidence"}]}
//
// T74 adds the wire="openai-chat" transport for OpenRouter-style
// OpenAI-compatible chat backends (model "~typesafe/jev-latest", verified
// 2026-09-30 via Exa against openrouter.ai docs + live /api/v1/models):
//
//	POST {endpoint}                       # e.g. https://openrouter.ai/api/v1/chat/completions
//	Authorization: Bearer <key>
//	Content-Type: application/json
//	HTTP-Referer / X-OpenRouter-Title     # optional app-attribution headers ([ai.jev].http_referer/.title)
//	{"model": "<model>", "messages": [{"role": "system", ...}, {"role": "user", "content": "<state+questions JSON>"}]}
//	→ {"choices": [{"message": {"content": "{\"answers\": [...]}"}}]}
//
// The same state/questions JSON rides in the user message and the answers
// JSON is recovered from the reply content, so the question/answer format
// is single-source and the solver is transport-agnostic.
//
// Verified 2026-09-27 (Exa, public docs: typesafe.ai blog, docs.rs/typesafe-jev,
// jevtypesafeai.com, jevaiguide.com, jev-agent.com): the official TypeSafe
// endpoint additionally requires a "model" field and documents questions as a
// keyed map with instructions/criteria (answers keyed by id, noul without a
// confidence field). Those variants belong to other Jev-compatible backends;
// the shape above is this build's contract, and adapting to another backend
// is deliberately localized to this one file. Limits inherited from the
// public docs and NOT re-enforced here (an oversized state fails the call and
// the run routes to review): state ≤32K tokens, ≤255 choice options.
//
// Failure semantics: every non-2xx status, timeout or malformed body is an
// error — callers treat it as "Jev unavailable" and fall back to the LLM.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Question-type vocabulary of the Jev wire contract.
const (
	JevQuestionNoul   = "noul"   // calibrated yes/no
	JevQuestionChoice = "choice" // one option out of Options
	JevQuestionScore  = "score"  // rating on ordered levels (contract-complete; unused by T33 tasks)
)

// jevTimeout is the fixed prototype transport budget: a prefilter that is
// slower than this loses to the LLM path it is meant to accelerate.
const jevTimeout = 5 * time.Second

// jevMaxResponseBytes bounds the decoded body (defensive read cap).
const jevMaxResponseBytes = 1 << 20

// JevQuestion is one typed question about the state.
type JevQuestion struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"` // noul | choice | score
	Prompt  string   `json:"prompt"`
	Options []string `json:"options,omitempty"` // choice only
}

// JevRequest is the POST body. State is the compact JSON serialization of the
// task input (a string per the T33 contract). Model is sent only when the
// client is configured with one (T74; empty = the T33 shape).
type JevRequest struct {
	Model     string        `json:"model,omitempty"`
	State     string        `json:"state"`
	Questions []JevQuestion `json:"questions"`
}

// JevAnswer is one calibrated answer; Value is kept raw because the contract
// allows an option string (choice), a yes/no boolean or number (noul/score).
type JevAnswer struct {
	ID          string          `json:"id"`
	Value       json.RawMessage `json:"value"`
	Probability float64         `json:"probability"`
	Confidence  float64         `json:"confidence"`
}

// JevResponse is the POST reply.
type JevResponse struct {
	Answers []JevAnswer `json:"answers"`
}

// JevClientOptions carries the T74 transport knobs: the wire encoding, the
// optional model name and the optional OpenRouter attribution headers
// (openai-chat wire only).
type JevClientOptions struct {
	Wire        string // "" | "systemone" | "openai-chat" ("" = systemone)
	Model       string // model name; empty = sent on neither wire
	HTTPReferer string // OpenRouter HTTP-Referer header when non-empty
	XTitle      string // OpenRouter X-OpenRouter-Title header when non-empty
}

// JevClient posts question batches to the Jev-compatible endpoint. It is
// safe for concurrent use; the atomic counters are the rate-limit/health
// metrics (JevStats) surfaced for monitoring.
type JevClient struct {
	endpoint string
	apiKey   string
	opts     JevClientOptions
	hc       *http.Client

	requests atomic.Int64 // POSTs attempted
	limited  atomic.Int64 // HTTP 429 responses (rate limit)
	errors   atomic.Int64 // failed calls (transport, status, decode)
}

// NewJevClient builds a client for one Jev-compatible endpoint.
func NewJevClient(endpoint, apiKey string, opts JevClientOptions) *JevClient {
	return &JevClient{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		opts:     opts,
		hc:       &http.Client{Timeout: jevTimeout},
	}
}

// Solve sends one question batch and returns the parsed answers. Every
// enrolled question must come back answered — a partial reply is an error
// (the caller routes to review instead of trusting a half-decision).
func (c *JevClient) Solve(ctx context.Context, req JevRequest) (JevResponse, error) {
	c.requests.Add(1)
	if c.opts.Model != "" { // T74: empty model = the T33 shape, no field
		req.Model = c.opts.Model
	}
	var out JevResponse
	var err error
	switch c.opts.Wire {
	case "openai-chat":
		out, err = c.solveOpenAIChat(ctx, req)
	default:
		out, err = c.solveSystemOne(ctx, req)
	}
	if err != nil {
		return JevResponse{}, err
	}
	if err := jevCheckCoverage(req, out); err != nil {
		c.errors.Add(1)
		return JevResponse{}, err
	}
	return out, nil
}

// post is the shared transport: one POST with Bearer auth, the optional
// T74 attribution headers, and the status/decode handling.
func (c *JevClient) post(ctx context.Context, body []byte) (io.ReadCloser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jev: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.opts.HTTPReferer != "" {
		httpReq.Header.Set("HTTP-Referer", c.opts.HTTPReferer)
	}
	if c.opts.XTitle != "" {
		httpReq.Header.Set("X-OpenRouter-Title", c.opts.XTitle)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jev: endpoint unreachable: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		c.limited.Add(1)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close() //nolint:errcheck // read-side only
		return nil, fmt.Errorf("jev: endpoint returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// solveSystemOne is the T33 transport: the {"state","questions"} body is
// the reply body verbatim.
func (c *JevClient) solveSystemOne(ctx context.Context, req JevRequest) (JevResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: encode request: %w", err)
	}
	rdr, err := c.post(ctx, body)
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, err
	}
	defer rdr.Close() //nolint:errcheck // read-side only
	var out JevResponse
	if err := json.NewDecoder(io.LimitReader(rdr, jevMaxResponseBytes)).Decode(&out); err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: decode response: %w", err)
	}
	return out, nil
}

// jevChatSystemPrompt instructs the chat backend to reply with the answers
// JSON only (the same answers contract as the systemone wire).
const jevChatSystemPrompt = `You are a calibration classifier. The user message carries a JSON object with "state" (the task input) and "questions" (each with id, type, prompt and optional options). Reply with ONLY a JSON object {"answers":[{"id":"<question id>","value":<answer value: option string or index for choice, boolean or number otherwise>,"probability":<0..1>,"confidence":<0..1>}]}, exactly one answer per question, no prose, no markdown.`

// openaiChatRequest/response are the OpenRouter-compatible chat/completions
// envelopes (OpenAI-normalized: choices is always an array).
type openaiChatRequest struct {
	Model    string              `json:"model"`
	Messages []openaiChatMessage `json:"messages"`
}

type openaiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// solveOpenAIChat is the T74 transport: the same state/questions JSON rides
// in the user message, the answers JSON is recovered from the reply
// content (fenced or bare).
func (c *JevClient) solveOpenAIChat(ctx context.Context, req JevRequest) (JevResponse, error) {
	payload, err := json.Marshal(openaiChatRequest{
		Model: req.Model,
		Messages: []openaiChatMessage{
			{Role: "system", Content: jevChatSystemPrompt},
			{Role: "user", Content: string(mustJevUserContent(req))},
		},
	})
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: encode request: %w", err)
	}
	rdr, err := c.post(ctx, payload)
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, err
	}
	defer rdr.Close() //nolint:errcheck // read-side only
	var chat openaiChatResponse
	if err := json.NewDecoder(io.LimitReader(rdr, jevMaxResponseBytes)).Decode(&chat); err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: decode response: %w", err)
	}
	if len(chat.Choices) == 0 || chat.Choices[0].Message.Content == "" {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: chat reply carries no content")
	}
	raw, ok := ExtractJSON(chat.Choices[0].Message.Content)
	if !ok {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: chat reply is not valid answers JSON")
	}
	var out JevResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: decode answers: %w", err)
	}
	return out, nil
}

// mustJevUserContent marshals the state+questions JSON for the user
// message (model stripped — it is a chat envelope field, not part of the
// answers contract).
func mustJevUserContent(req JevRequest) []byte {
	req.Model = ""
	body, err := json.Marshal(req)
	if err != nil { // strings and slices of plain structs cannot fail
		panic(fmt.Sprintf("jev: marshal user content: %v", err))
	}
	return body
}

// jevCheckCoverage requires exactly one answer per question id.
func jevCheckCoverage(req JevRequest, out JevResponse) error {
	seen := make(map[string]int, len(out.Answers))
	for _, a := range out.Answers {
		seen[a.ID]++
	}
	for _, q := range req.Questions {
		switch seen[q.ID] {
		case 0:
			return fmt.Errorf("jev: no answer for question %q", q.ID)
		case 1:
		default:
			return fmt.Errorf("jev: duplicate answers for question %q", q.ID)
		}
	}
	return nil
}

// JevStats is the metrics snapshot of one client (rate-limit monitoring).
type JevStats struct {
	Requests    int64 `json:"requests"`
	RateLimited int64 `json:"rate_limited"`
	Errors      int64 `json:"errors"`
}

// Stats returns the current counter values.
func (c *JevClient) Stats() JevStats {
	return JevStats{
		Requests:    c.requests.Load(),
		RateLimited: c.limited.Load(),
		Errors:      c.errors.Load(),
	}
}
