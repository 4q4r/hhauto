package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hhauto/internal/config"
)

// Protocol facts (2captcha API v1, https://2captcha.com/2captcha-api):
//   - POST https://<base>/in.php with key, json=1 and, per challenge type:
//     method=base64 + body=<base64 image> | method=hcaptcha|turnstile +
//     sitekey=<key>&pageurl=<url>. Reply: {"status":1,"request":"<id>"}
//     or {"status":0,"request":"ERROR_*"}.
//   - GET https://<base>/res.php?key=...&action=get&id=...&json=1 until the
//     reply is not {"status":0,"request":"CAPCHA_NOT_READY"}; solved replies
//     {"status":1,"request":"<token>"}. Other status=0 values are errors.
//   - GET https://<base>/res.php?key=...&action=getbalance&json=1 replies
//     {"status":1,"request":"<balance as string>"}.
//
// The same protocol is spoken by RuCaptcha, SolveCaptcha and other clones —
// only base_url differs, which is why BaseURL is configurable.
const (
	defaultTwoCaptchaBaseURL = "https://2captcha.com"
	defaultPollIntervalMS    = 5000
	defaultTimeoutS          = 120
)

type twoCaptchaSolver struct {
	id           string
	baseURL      string // scheme://host, no trailing slash
	keys         keySource
	pollInterval time.Duration
	solveTimeout time.Duration

	client httpJSONClient
	logger *slog.Logger
}

// newTwoCaptcha builds a Solver speaking the 2captcha in.php/res.php protocol.
// The API key is resolved lazily: KeyEnv first, falling back to the inline
// cfg.Key with a WARN (the fallback is discouraged; the config validator
// warns too). A provider with neither key source is a configuration error.
func newTwoCaptcha(id string, cfg config.CaptchaProvider) (*twoCaptchaSolver, error) {
	if cfg.PollMS < 0 {
		return nil, fmt.Errorf("captcha: %s: poll_interval_ms must be >= 0", id)
	}
	if cfg.TimeoutS < 0 {
		return nil, fmt.Errorf("captcha: %s: timeout_s must be >= 0", id)
	}
	keys, err := newKeySource(id, cfg)
	if err != nil {
		return nil, err
	}
	base, err := normalizeBaseURL(cfg.BaseURL, defaultTwoCaptchaBaseURL)
	if err != nil {
		return nil, fmt.Errorf("captcha: %s: %w", id, err)
	}

	return &twoCaptchaSolver{
		id:           id,
		baseURL:      base,
		keys:         keys,
		pollInterval: pollDuration(cfg.PollMS),
		solveTimeout: timeoutDuration(cfg.TimeoutS),
		client:       newHTTPJSONClient(),
		logger:       slog.Default(),
	}, nil
}

func (s *twoCaptchaSolver) ID() string { return s.id }

func (s *twoCaptchaSolver) Solve(ctx context.Context, ch Challenge) (Solution, error) {
	start := time.Now()
	key, err := s.keys.get(s.logger)
	if err != nil {
		return Solution{}, err
	}

	submitForm, err := s.submitForm(ch)
	if err != nil {
		return Solution{}, err
	}
	submitForm.Set("key", key)
	submitForm.Set("json", "1")

	data, err := s.client.do(ctx, "POST", s.baseURL+"/in.php", submitForm)
	if err != nil {
		return Solution{}, err
	}
	var sub struct {
		Status  int    `json:"status"`
		Request string `json:"request"`
	}
	if err := decodeJSON(data, &sub); err != nil {
		return Solution{}, fmt.Errorf("captcha: %s: in.php: %w", s.id, err)
	}
	if sub.Status != 1 {
		return Solution{}, &ProtocolError{ProviderID: s.id, Code: sub.Request}
	}

	deadline := time.Now().Add(s.solveTimeout)
	for {
		if err := waitFor(ctx, s.pollInterval); err != nil {
			return Solution{}, fmt.Errorf("captcha: %s: %w", s.id, err)
		}
		sol, retry, err := s.pollOnce(ctx, key, sub.Request, start)
		if err != nil {
			return Solution{}, err
		}
		if !retry {
			return sol, nil
		}
		if time.Now().After(deadline) {
			return Solution{}, fmt.Errorf("captcha: %s: solve timeout after %s (task %s)",
				s.id, s.solveTimeout, sub.Request)
		}
	}
}

// pollOnce performs a single res.php action=get call. retry reports
// CAPCHA_NOT_READY (keep polling); err covers protocol and transport faults.
func (s *twoCaptchaSolver) pollOnce(ctx context.Context, key, taskID string, start time.Time) (Solution, bool, error) {
	q := url.Values{}
	q.Set("key", key)
	q.Set("action", "get")
	q.Set("id", taskID)
	q.Set("json", "1")

	data, err := s.client.do(ctx, "GET", s.baseURL+"/res.php?"+q.Encode(), nil)
	if err != nil {
		return Solution{}, false, err
	}
	var res struct {
		Status  int    `json:"status"`
		Request string `json:"request"`
	}
	if err := decodeJSON(data, &res); err != nil {
		return Solution{}, false, fmt.Errorf("captcha: %s: res.php: %w", s.id, err)
	}
	if res.Status == 1 {
		return Solution{Token: res.Request, ProviderID: s.id, Latency: time.Since(start)}, false, nil
	}
	if res.Request == "CAPCHA_NOT_READY" {
		return Solution{}, true, nil
	}
	return Solution{}, false, &ProtocolError{ProviderID: s.id, Code: res.Request}
}

func (s *twoCaptchaSolver) Balance(ctx context.Context) (float64, error) {
	key, err := s.keys.get(s.logger)
	if err != nil {
		return 0, err
	}
	q := url.Values{}
	q.Set("key", key)
	q.Set("action", "getbalance")
	q.Set("json", "1")

	data, err := s.client.do(ctx, "GET", s.baseURL+"/res.php?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	var res struct {
		Status  int    `json:"status"`
		Request string `json:"request"`
	}
	if err := decodeJSON(data, &res); err != nil {
		return 0, fmt.Errorf("captcha: %s: getbalance: %w", s.id, err)
	}
	if res.Status != 1 {
		return 0, &ProtocolError{ProviderID: s.id, Code: res.Request}
	}
	bal, err := strconv.ParseFloat(res.Request, 64)
	if err != nil {
		return 0, fmt.Errorf("captcha: %s: getbalance: unparsable balance %q", s.id, res.Request)
	}
	return bal, nil
}

// submitForm maps a Challenge onto in.php protocol parameters and validates
// that required fields are present before anything is sent to the service.
func (s *twoCaptchaSolver) submitForm(ch Challenge) (url.Values, error) {
	form := url.Values{}
	switch ch.Type {
	case TypeImage:
		if ch.ImageB64 == "" {
			return nil, fmt.Errorf("captcha: %s: image challenge requires image_b64", s.id)
		}
		form.Set("method", "base64")
		form.Set("body", ch.ImageB64)
	case TypeHCaptcha, TypeTurnstile:
		if ch.SiteKey == "" || ch.PageURL == "" {
			return nil, fmt.Errorf("captcha: %s: %s challenge requires sitekey and pageurl", s.id, ch.Type)
		}
		form.Set("method", ch.Type) // protocol method names match our type ids
		form.Set("sitekey", ch.SiteKey)
		form.Set("pageurl", ch.PageURL)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, ch.Type)
	}
	return form, nil
}

// normalizeBaseURL validates and normalizes a provider base_url: absolute
// http(s) URL, no path, no trailing slash; empty falls back to def.
func normalizeBaseURL(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid base_url scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid base_url %q: missing host", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("invalid base_url %q: path is not allowed", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/"), nil
}

// decodeJSON strictly unmarshals a protocol response.
func decodeJSON(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("invalid protocol response: %w", err)
	}
	return nil
}
