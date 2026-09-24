package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"hhauto/internal/config"
)

// Protocol facts (anti-captcha, https://anti-captcha.com/apidoc):
//   - POST https://<base>/createTask  {"clientKey","task"} →
//     {"errorId":0,"taskId":N} or errorId>0 with errorCode/errorDescription.
//   - POST https://<base>/getTaskResult {"clientKey","taskId"} → poll every
//     2–5s until {"errorId":0,"status":"ready","solution":{...}}; while
//     "status":"processing" keep polling. errorId>0 carries errorCode.
//   - POST https://<base>/getBalance {"clientKey"} → {"errorId":0,"balance":f}.
//   - Task types used here: ImageToTextTask{body}, HCaptchaTaskProxyless and
//     TurnstileTaskProxyless{websiteURL,websiteKey}. Solutions: text /
//     gRecaptchaResponse / token respectively.
//
// CapMonster Cloud and other clones speak this protocol with a different
// base_url, which is why BaseURL is configurable.
const defaultAntiCaptchaBaseURL = "https://api.anti-captcha.com"

type antiCaptchaSolver struct {
	id           string
	baseURL      string
	keys         keySource
	pollInterval time.Duration
	solveTimeout time.Duration

	client httpJSONClient
}

// newAntiCaptcha builds a Solver speaking the anti-captcha createTask/
// getTaskResult protocol. Key resolution mirrors the 2captcha adapter:
// KeyEnv first, inline cfg.Key as the warned fallback, neither = config error.
func newAntiCaptcha(id string, cfg config.CaptchaProvider, logger *slog.Logger) (*antiCaptchaSolver, error) {
	if logger == nil {
		logger = slog.Default()
	}
	solver := &antiCaptchaSolver{
		id:     id,
		client: newHTTPJSONClient(),
	}
	if err := solver.configure(cfg, logger); err != nil {
		return nil, err
	}
	return solver, nil
}

func (s *antiCaptchaSolver) configure(cfg config.CaptchaProvider, logger *slog.Logger) error {
	if cfg.PollMS < 0 {
		return fmt.Errorf("captcha: %s: poll_interval_ms must be >= 0", s.id)
	}
	if cfg.TimeoutS < 0 {
		return fmt.Errorf("captcha: %s: timeout_s must be >= 0", s.id)
	}
	keys, err := newKeySource(s.id, cfg, logger)
	if err != nil {
		return err
	}
	base, err := normalizeBaseURL(cfg.BaseURL, defaultAntiCaptchaBaseURL)
	if err != nil {
		return fmt.Errorf("captcha: %s: %w", s.id, err)
	}

	s.baseURL = base
	s.keys = keys
	s.pollInterval = pollDuration(cfg.PollMS)
	s.solveTimeout = timeoutDuration(cfg.TimeoutS)
	return nil
}

func (s *antiCaptchaSolver) ID() string { return s.id }

func (s *antiCaptchaSolver) Solve(ctx context.Context, ch Challenge) (Solution, error) {
	start := time.Now()
	key, err := s.keys.get()
	if err != nil {
		return Solution{}, err
	}

	task, err := s.taskObject(ch)
	if err != nil {
		return Solution{}, err
	}

	var create struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           int64  `json:"taskId"`
	}
	if err := s.post(ctx, "/createTask", map[string]any{
		"clientKey": key,
		"task":      task,
	}, &create); err != nil {
		return Solution{}, err
	}
	if err := acCheck(s.id, create.ErrorID, create.ErrorCode, create.ErrorDescription); err != nil {
		return Solution{}, err
	}
	if create.TaskID <= 0 {
		return Solution{}, fmt.Errorf("captcha: %s: createTask returned no taskId", s.id)
	}

	deadline := time.Now().Add(s.solveTimeout)
	for {
		if err := waitFor(ctx, s.pollInterval); err != nil {
			return Solution{}, fmt.Errorf("captcha: %s: %w", s.id, err)
		}

		var res struct {
			ErrorID          int            `json:"errorId"`
			ErrorCode        string         `json:"errorCode"`
			ErrorDescription string         `json:"errorDescription"`
			Status           string         `json:"status"`
			Solution         map[string]any `json:"solution"`
		}
		if err := s.post(ctx, "/getTaskResult", map[string]any{
			"clientKey": key,
			"taskId":    create.TaskID,
		}, &res); err != nil {
			return Solution{}, err
		}
		if err := acCheck(s.id, res.ErrorID, res.ErrorCode, res.ErrorDescription); err != nil {
			return Solution{}, err
		}

		switch res.Status {
		case "ready":
			token, err := solutionToken(ch.Type, res.Solution)
			if err != nil {
				return Solution{}, fmt.Errorf("captcha: %s: %w", s.id, err)
			}
			return Solution{Token: token, ProviderID: s.id, Latency: time.Since(start)}, nil
		case "processing":
			// keep polling
		default:
			return Solution{}, fmt.Errorf("captcha: %s: unexpected task status %q", s.id, res.Status)
		}

		if time.Now().After(deadline) {
			return Solution{}, fmt.Errorf("captcha: %s: solve timeout after %s (task %d)",
				s.id, s.solveTimeout, create.TaskID)
		}
	}
}

func (s *antiCaptchaSolver) Balance(ctx context.Context) (float64, error) {
	key, err := s.keys.get()
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrorID          int     `json:"errorId"`
		ErrorCode        string  `json:"errorCode"`
		ErrorDescription string  `json:"errorDescription"`
		Balance          float64 `json:"balance"`
	}
	if err := s.post(ctx, "/getBalance", map[string]any{"clientKey": key}, &res); err != nil {
		return 0, err
	}
	if err := acCheck(s.id, res.ErrorID, res.ErrorCode, res.ErrorDescription); err != nil {
		return 0, err
	}
	return res.Balance, nil
}

// post sends one JSON request and decodes the JSON reply.
func (s *antiCaptchaSolver) post(ctx context.Context, path string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("captcha: %s: encode request: %w", s.id, err)
	}
	data, err := s.client.postBody(ctx, "POST", s.baseURL+path, body)
	if err != nil {
		return err
	}
	if err := decodeJSON(data, result); err != nil {
		return fmt.Errorf("captcha: %s: %s: %w", s.id, path, err)
	}
	return nil
}

// acCheck converts errorId>0 into a typed ProtocolError.
func acCheck(id string, errorID int, code, description string) error {
	if errorID == 0 {
		return nil
	}
	if code == "" {
		code = fmt.Sprintf("ERROR_ID_%d", errorID)
	}
	return &ProtocolError{ProviderID: id, Code: code, Description: description}
}

// taskObject maps a Challenge onto an anti-captcha task object and validates
// required fields before anything is sent to the service.
func (s *antiCaptchaSolver) taskObject(ch Challenge) (map[string]any, error) {
	task := map[string]any{}
	switch ch.Type {
	case TypeImage:
		if ch.ImageB64 == "" {
			return nil, fmt.Errorf("captcha: %s: image challenge requires image_b64", s.id)
		}
		task["type"] = "ImageToTextTask"
		task["body"] = ch.ImageB64
	case TypeHCaptcha:
		if ch.SiteKey == "" || ch.PageURL == "" {
			return nil, fmt.Errorf("captcha: %s: hcaptcha challenge requires sitekey and pageurl", s.id)
		}
		task["type"] = "HCaptchaTaskProxyless"
		task["websiteURL"] = ch.PageURL
		task["websiteKey"] = ch.SiteKey
	case TypeTurnstile:
		if ch.SiteKey == "" || ch.PageURL == "" {
			return nil, fmt.Errorf("captcha: %s: turnstile challenge requires sitekey and pageurl", s.id)
		}
		task["type"] = "TurnstileTaskProxyless"
		task["websiteURL"] = ch.PageURL
		task["websiteKey"] = ch.SiteKey
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, ch.Type)
	}
	return task, nil
}

// solutionToken extracts the token from a ready solution object. Clones
// sometimes use the alternative field name, so both spellings are accepted.
func solutionToken(challengeType string, solution map[string]any) (string, error) {
	if solution == nil {
		return "", fmt.Errorf("empty solution")
	}
	primary, fallback := "token", "gRecaptchaResponse"
	switch challengeType {
	case TypeImage:
		primary = "text"
		fallback = ""
	}
	for _, field := range []string{primary, fallback} {
		if field == "" {
			continue
		}
		if v, ok := solution[field].(string); ok && strings.TrimSpace(v) != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("solution for %s has no usable token field", challengeType)
}

// keySource resolves the provider API key per request: KeyEnv first, the
// inline key as a fallback. It is immutable after construction — the
// inline-key warning is emitted once by newKeySource (the fact is known at
// configure time), so concurrent Solve calls share it without locks.
// The key value is never logged.
type keySource struct {
	providerID string
	keyEnv     string
	inline     string
}

func newKeySource(id string, cfg config.CaptchaProvider, logger *slog.Logger) (keySource, error) {
	if cfg.KeyEnv == "" && cfg.Key == "" {
		return keySource{}, fmt.Errorf("captcha: %s: no API key configured (set key_env)", id)
	}
	if cfg.Key != "" {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("captcha: inline API key configured (discouraged; put the key in key_env instead)",
			"provider", id, "key_env", cfg.KeyEnv)
	}
	return keySource{
		providerID: id,
		keyEnv:     cfg.KeyEnv,
		inline:     cfg.Key,
	}, nil
}

func (k keySource) get() (string, error) {
	if k.keyEnv != "" {
		if v := os.Getenv(k.keyEnv); v != "" {
			return v, nil
		}
	}
	if k.inline != "" {
		return k.inline, nil
	}
	return "", fmt.Errorf("captcha: %s: API key unavailable (env %s is not set)", k.providerID, k.keyEnv)
}

// pollDuration maps config.PollMS onto a duration with the 5000ms default.
func pollDuration(pollMS int) time.Duration {
	if pollMS <= 0 {
		return defaultPollIntervalMS * time.Millisecond
	}
	return time.Duration(pollMS) * time.Millisecond
}

// timeoutDuration maps config.TimeoutS onto a duration with the 120s default.
func timeoutDuration(timeoutS int) time.Duration {
	if timeoutS <= 0 {
		return defaultTimeoutS * time.Second
	}
	return time.Duration(timeoutS) * time.Second
}
