package ai

// JevClient is the transport for the optional Jev (System One) prefilter
// (T33): one POST to the configured Jev-compatible endpoint, Bearer auth, a
// fixed short timeout and typed request/response structs.
//
// Wire contract (T33 owner-specified live contract, implemented verbatim):
//
//	POST {endpoint}                       # [ai.jev].endpoint (default https://api.typesafe.ai/v1/systemone)
//	Authorization: Bearer <key>           # key read from [ai.jev].api_key_env
//	Content-Type: application/json
//	{"state": "<compact JSON>", "questions": [{"id", "type": "noul"|"choice"|"score", "prompt", "options"?}]}
//	→ {"answers": [{"id", "value", "probability", "confidence"}]}
//
// Verified 2026-09-27 (Exa, public docs: typesafe.ai blog, docs.rs/typesafe-jev,
// jevtypesafeai.com, jevaiguide.com, jev-agent.com): the official TypeSafe
// endpoint additionally requires a "model" field and documents questions as a
// keyed map with instructions/criteria (answers keyed by id, noul without a
// confidence field). Those variants belong to other Jev-compatible backends;
// the shape above is this build's contract, and adapting to another backend
// is deliberately localized to this one file. Limits inherited from the
// public docs and NOT re-enforced here (an oversized state fails the call and
// the run escalates to the LLM): state ≤32K tokens, ≤255 choice options.
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
// task input (a string per the T33 contract).
type JevRequest struct {
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

// JevClient posts question batches to the Jev-compatible endpoint. It is
// safe for concurrent use; the atomic counters are the rate-limit/health
// metrics (JevStats) surfaced for monitoring.
type JevClient struct {
	endpoint string
	apiKey   string
	hc       *http.Client

	requests atomic.Int64 // POSTs attempted
	limited  atomic.Int64 // HTTP 429 responses (rate limit)
	errors   atomic.Int64 // failed calls (transport, status, decode)
}

// NewJevClient builds a client for one Jev-compatible endpoint.
func NewJevClient(endpoint, apiKey string) *JevClient {
	return &JevClient{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		hc:       &http.Client{Timeout: jevTimeout},
	}
}

// Solve sends one question batch and returns the parsed answers. Every
// enrolled question must come back answered — a partial reply is an error
// (the caller escalates to the LLM instead of trusting a half-decision).
func (c *JevClient) Solve(ctx context.Context, req JevRequest) (JevResponse, error) {
	c.requests.Add(1)
	body, err := json.Marshal(req)
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: endpoint unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-side only
	if resp.StatusCode == http.StatusTooManyRequests {
		c.limited.Add(1)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: endpoint returned HTTP %d", resp.StatusCode)
	}
	var out JevResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, jevMaxResponseBytes)).Decode(&out); err != nil {
		c.errors.Add(1)
		return JevResponse{}, fmt.Errorf("jev: decode response: %w", err)
	}
	if err := jevCheckCoverage(req, out); err != nil {
		c.errors.Add(1)
		return JevResponse{}, err
	}
	return out, nil
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
