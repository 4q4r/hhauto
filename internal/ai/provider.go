package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hhauto/internal/config"
)

// Dialect identifiers (config ai.providers.<id>.dialect).
const (
	DialectOpenAIChat        = "openai-chat"
	DialectOpenAIResponses   = "openai-responses"
	DialectAnthropicMessages = "anthropic-messages"
)

// EffortNone requests no reasoning/thinking at all.
const EffortNone = "none"

// Default request limits.
const (
	defaultProviderTimeout    = 120 * time.Second
	defaultAnthropicMaxTokens = 2048 // Messages API requires max_tokens
	retryAttempts             = 4    // initial try + 3 retries
	maxResponseBytes          = 8 << 20
	anthropicVersion          = "2023-06-01"
	maxRetryAfter             = 60 * time.Second
)

// APIError is a typed provider failure. Status 0 means a network/transport
// error. Retryable marks transient failures (429/5xx/network).
type APIError struct {
	ProviderID string
	Dialect    string
	Model      string
	Status     int
	Code       string
	Param      string
	Message    string
	Retryable  bool
	RetryAfter time.Duration // from Retry-After on 429
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("ai: %s/%s: network error: %s", e.ProviderID, e.Dialect, e.Message)
	}
	return fmt.Sprintf("ai: %s/%s: HTTP %d: %s", e.ProviderID, e.Dialect, e.Status, e.Message)
}

// temperatureUnsupported reports whether the API rejected the temperature
// parameter (reasoning models do). Bodies follow the OpenAI error shape;
// Anthropic's message text is matched as a fallback.
func (e *APIError) temperatureUnsupported() bool {
	if e.Status != http.StatusBadRequest {
		return false
	}
	if (e.Code == "unsupported_parameter" || e.Code == "unknown_parameter") &&
		strings.Contains(strings.ToLower(e.Param), "temperature") {
		return true
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "temperature") &&
		(strings.Contains(m, "not supported") || strings.Contains(m, "unsupported") ||
			strings.Contains(m, "unexpected") || strings.Contains(m, "unrecognized"))
}

// thinkingUnsupported reports whether the model rejected adaptive thinking /
// effort controls; the retry then runs without them.
func (e *APIError) thinkingUnsupported() bool {
	if e.Status != http.StatusBadRequest {
		return false
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "thinking") || strings.Contains(m, "output_config") ||
		strings.Contains(m, "effort")
}

// ModelProfiles remembers per-model quirks discovered at runtime (e.g. a
// reasoning model that rejects temperature) so later requests skip the
// failing parameter. Safe for concurrent use.
type ModelProfiles struct {
	mu     sync.Mutex
	noTemp map[string]bool
}

// NewModelProfiles builds an empty model-profile registry.
func NewModelProfiles() *ModelProfiles {
	return &ModelProfiles{noTemp: map[string]bool{}}
}

// TemperatureUnsupported reports whether the model rejects the temperature
// parameter (learned from a provider rejection and cached in-process).
func (p *ModelProfiles) TemperatureUnsupported(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.noTemp[model]
}

// SetTemperatureUnsupported records that the model rejects the temperature
// parameter.
func (p *ModelProfiles) SetTemperatureUnsupported(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.noTemp[model] = true
}

// providerDeps carries the injectable plumbing shared by all dialects.
type providerDeps struct {
	client   *http.Client
	logger   *slog.Logger
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	profiles *ModelProfiles
}

// Option customizes provider internals (tests inject fakes; production uses
// the defaults: TLS verification is always on, no option can disable it).
type Option func(*providerDeps)

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(d *providerDeps) { d.client = c } }

// WithLogger sets the provider logger.
func WithLogger(l *slog.Logger) Option { return func(d *providerDeps) { d.logger = l } }

// WithNow overrides the clock.
func WithNow(f func() time.Time) Option { return func(d *providerDeps) { d.now = f } }

// WithSleep overrides backoff sleeping.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(d *providerDeps) { d.sleep = f }
}

// WithProfiles shares the model-quirk memory across providers.
func WithProfiles(p *ModelProfiles) Option { return func(d *providerDeps) { d.profiles = p } }

func (d *providerDeps) withDefaults() providerDeps {
	out := *d
	if out.client == nil {
		out.client = &http.Client{} // default transport: certificate verification non-negotiable
	}
	if out.logger == nil {
		out.logger = slog.Default()
	}
	if out.now == nil {
		out.now = time.Now
	}
	if out.sleep == nil {
		out.sleep = waitFor
	}
	if out.profiles == nil {
		out.profiles = NewModelProfiles()
	}
	return out
}

// baseProvider is the shared transport of the three dialect adapters.
type baseProvider struct {
	id        string
	dialect   string
	baseURL   string
	apiKeyEnv string
	timeout   time.Duration
	deps      providerDeps
}

func newBaseProvider(id string, cfg config.AIProvider, dialect string, deps providerDeps) (baseProvider, error) {
	if cfg.Dialect != "" && cfg.Dialect != dialect {
		return baseProvider{}, fmt.Errorf("ai: %s: dialect %q must be %q", id, cfg.Dialect, dialect)
	}
	if cfg.BaseURL == "" {
		return baseProvider{}, fmt.Errorf("ai: %s: base_url is required", id)
	}
	deps = deps.withDefaults()
	return baseProvider{
		id:        id,
		dialect:   dialect,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		apiKeyEnv: cfg.APIKeyEnv,
		timeout:   timeoutDuration(cfg.TimeoutS),
		deps:      deps,
	}, nil
}

func timeoutDuration(sec int) time.Duration {
	if sec <= 0 {
		return defaultProviderTimeout
	}
	return time.Duration(sec) * time.Second
}

// apiKey resolves the credential from the environment at call time; the
// value never reaches logs or errors.
func (b *baseProvider) apiKey() (string, error) {
	if b.apiKeyEnv == "" {
		return "", fmt.Errorf("ai: %s: no api_key_env configured", b.id)
	}
	key := os.Getenv(b.apiKeyEnv)
	if key == "" {
		return "", fmt.Errorf("ai: %s: API key unavailable (env %s is not set)", b.id, b.apiKeyEnv)
	}
	return key, nil
}

// Dialect returns the dialect identifier.
func (b *baseProvider) Dialect() string { return b.dialect }

// postJSON performs one dialect request with retry: 429 honors Retry-After,
// 5xx and network errors back off 1s/2s/4s, everything else is terminal.
func (b *baseProvider) postJSON(ctx context.Context, model string, path string, headers map[string]string, payload []byte) ([]byte, error) {
	url := b.baseURL + path
	var lastErr error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			if err := b.deps.sleep(ctx, b.backoff(attempt, lastErr)); err != nil {
				return nil, fmt.Errorf("ai: %s: %w", b.id, err)
			}
		}
		body, err := b.once(ctx, model, url, headers, payload)
		if err == nil {
			return body, nil
		}
		lastErr = err
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable || attempt == retryAttempts-1 {
			return nil, err
		}
	}
	return nil, lastErr
}

// backoff computes the wait before the given retry attempt with full jitter
// (uniform in [0, base], AWS-style): equal-load collisions retry in
// different phases instead of marching in lockstep. A server-mandated
// Retry-After is honored exactly.
func (b *baseProvider) backoff(attempt int, lastErr error) time.Duration {
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	shift := attempt - 1
	if shift > 2 {
		shift = 2
	}
	base := time.Second << shift
	return time.Duration(rand.Int63n(int64(base)))
}

// once performs exactly one HTTP round trip and converts failures to APIError.
func (b *baseProvider) once(ctx context.Context, model, url string, headers map[string]string, payload []byte) ([]byte, error) {
	perAttempt, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(perAttempt, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ai: %s: build request: %w", b.id, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.deps.client.Do(req)
	if err != nil {
		return nil, &APIError{ProviderID: b.id, Dialect: b.dialect, Model: model, Message: err.Error(), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }() // body consumed below; close error is not actionable
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode == http.StatusTooManyRequests {
		e := &APIError{
			ProviderID: b.id, Dialect: b.dialect, Model: model, Status: resp.StatusCode,
			Message: "rate limited", Retryable: true,
			RetryAfter: retryAfterOf(resp.Header.Get("Retry-After")),
		}
		if readErr == nil {
			if m := errorMessageOf(data); m != "" {
				e.Message = m
			}
		}
		return nil, e
	}
	if resp.StatusCode >= 500 {
		e := &APIError{ProviderID: b.id, Dialect: b.dialect, Model: model, Status: resp.StatusCode, Message: "server error", Retryable: true}
		if readErr == nil {
			if m := errorMessageOf(data); m != "" {
				e.Message = m
			}
		}
		return nil, e
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &APIError{ProviderID: b.id, Dialect: b.dialect, Model: model, Status: resp.StatusCode, Message: fmt.Sprintf("unexpected status %d", resp.StatusCode)}
		if readErr == nil {
			ec, param, m := errorFieldsOf(data)
			e.Code, e.Param, e.Message = ec, param, m
		}
		return nil, e
	}
	if readErr != nil {
		return nil, &APIError{ProviderID: b.id, Dialect: b.dialect, Model: model, Message: "read response: " + readErr.Error(), Retryable: true}
	}
	return data, nil
}

func retryAfterOf(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Second
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		d := time.Duration(sec) * time.Second
		return min(d, maxRetryAfter)
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return min(d, maxRetryAfter)
	}
	return time.Second
}

// errorFieldsOf extracts {error:{code,param,message}} (OpenAI shape) or
// {error:{type,message}} (Anthropic shape) from an error body.
func errorFieldsOf(data []byte) (code, param, message string) {
	var wrap struct {
		Error struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &wrap) == nil && wrap.Error.Message != "" {
		switch c := wrap.Error.Code.(type) {
		case string:
			code = c
		case float64:
			code = strconv.Itoa(int(c))
		}
		if code == "" {
			code = wrap.Error.Type
		}
		return code, wrap.Error.Param, wrap.Error.Message
	}
	return "", "", strings.TrimSpace(string(data))
}

func errorMessageOf(data []byte) string {
	_, _, m := errorFieldsOf(data)
	if len(m) > 300 {
		return m[:300]
	}
	return m
}

// waitFor sleeps for d or until ctx is done.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// modelCompleter is implemented by the dialect adapters so requests can be
// bound to a concrete model name.
type modelCompleter interface {
	completeModel(ctx context.Context, model string, req Request) (Response, error)
}

// BoundProvider is a Provider fixed to one model name from [ai.models].
// The request contract (ai.Request) deliberately carries no model field, so
// the Runner hands out one bound provider per model profile.
type BoundProvider struct {
	inner modelCompleter
	model string
}

// NewBoundProvider binds a dialect provider to a model name.
func NewBoundProvider(p Provider, model string) (Provider, error) {
	mc, ok := p.(modelCompleter)
	if !ok {
		return nil, fmt.Errorf("ai: provider %T cannot be bound to a model", p)
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("ai: model name is required for binding")
	}
	return &BoundProvider{inner: mc, model: model}, nil
}

// Complete implements the ai Runner surface for a model-bound provider.
func (b *BoundProvider) Complete(ctx context.Context, req Request) (Response, error) {
	return b.inner.completeModel(ctx, b.model, req)
}

// Dialect returns the wrapped provider's wire dialect.
func (b *BoundProvider) Dialect() string {
	if d, ok := b.inner.(interface{ Dialect() string }); ok {
		return d.Dialect()
	}
	return ""
}

// effortAllowedOpenAI validates the reasoning_effort enum documented for
// OpenAI Chat Completions and Responses (none, minimal, low, medium, high,
// xhigh, max).
func effortAllowedOpenAI(effort string) bool {
	switch effort {
	case "", EffortNone, "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// openAIHeaders are the shared auth headers of both OpenAI dialects.
func openAIHeaders(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key}
}

// schemaNameFor returns the strict-mode schema name for a task; OpenAI
// restricts it to [a-zA-Z0-9_-]{1,64}, which task names already satisfy.
func schemaNameFor(task TaskName) string {
	return SchemaName(task)
}
