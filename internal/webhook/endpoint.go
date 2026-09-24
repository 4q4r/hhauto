package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

const (
	// httpAttemptTimeout is the budget of a single HTTP delivery attempt;
	// retries restart it, so total time is bounded by the retry policy.
	httpAttemptTimeout = 10 * time.Second
	// telegramTextLimit keeps the message safely under the Bot API limit of
	// 4096 characters for sendMessage (see core.telegram.org/bots/api#sendmessage).
	telegramTextLimit = 3500
	// discordTextLimit is the documented maximum for webhook `content`
	// (see discord.com/developers/docs/resources/webhook#execute-webhook).
	discordTextLimit = 2000
)

// endpoint is one delivery target built from a single [webhook.<id>] config
// entry. It owns its HTTP client (direct or proxy-bound) and its retry policy.
type endpoint struct {
	id      string // stable outbox identifier: the endpoint URL
	cfg     config.Webhook
	secret  string // raw preset only
	client  *http.Client
	backoff func(attempt int) time.Duration // exponential 1s->2s->4s... by default
}

// newEndpoint validates one webhook config entry and resolves its
// dependencies. Any configuration problem (unknown preset, missing secret
// env, unresolvable proxy_ref) is an error: the caller must not deliver
// through a half-configured endpoint.
func newEndpoint(cfg config.Webhook, reg proxy.Registry) (*endpoint, error) {
	if cfg.URL == "" {
		return nil, errors.New("webhook: url is empty")
	}
	switch cfg.Preset {
	case "raw", "telegram", "discord":
	default:
		return nil, fmt.Errorf("webhook %s: unknown preset %q", cfg.URL, cfg.Preset)
	}

	ep := &endpoint{
		id:  cfg.URL,
		cfg: cfg,
		backoff: func(attempt int) time.Duration {
			return time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s, ...
		},
	}

	switch cfg.Preset {
	case "raw":
		if cfg.SecretEnv == "" {
			return nil, fmt.Errorf("webhook %s: secret_env is required for the raw preset", cfg.URL)
		}
		secret, ok := os.LookupEnv(cfg.SecretEnv)
		if !ok {
			return nil, fmt.Errorf("webhook %s: secret env %q is not set", cfg.URL, cfg.SecretEnv)
		}
		ep.secret = secret
	case "telegram":
		if cfg.ChatID == "" {
			return nil, fmt.Errorf("webhook %s: chat_id is required for the telegram preset", cfg.URL)
		}
	}

	client := &http.Client{Timeout: httpAttemptTimeout}
	if cfg.ProxyRef != "" {
		if reg == nil {
			return nil, fmt.Errorf("webhook %s: proxy_ref %q set but no proxy registry provided", cfg.URL, cfg.ProxyRef)
		}
		d, err := reg.Dialer(cfg.ProxyRef) // unknown ref => error, never direct fallback
		if err != nil {
			return nil, fmt.Errorf("webhook %s: resolve proxy_ref %q: %w", cfg.URL, cfg.ProxyRef, err)
		}
		client.Transport = &http.Transport{DialContext: d.DialContext}
	}
	ep.client = client

	return ep, nil
}

// subscribes reports whether the endpoint wants events of the given type.
// An empty Events filter means all events.
func (e *endpoint) subscribes(evType string) bool {
	if len(e.cfg.Events) == 0 {
		return true
	}
	return slices.Contains(e.cfg.Events, evType)
}

// buildPayload produces the exact HTTP body for this endpoint's preset.
func (e *endpoint) buildPayload(ev Event) ([]byte, error) {
	switch e.cfg.Preset {
	case "raw":
		return json.Marshal(ev)
	case "telegram":
		body := struct {
			ChatID                string `json:"chat_id"`
			Text                  string `json:"text"`
			ParseMode             string `json:"parse_mode"`
			DisableWebPagePreview bool   `json:"disable_web_page_preview"`
		}{
			ChatID:                e.cfg.ChatID,
			Text:                  html.EscapeString(truncateRunes(ev.Message, telegramTextLimit)),
			ParseMode:             "HTML",
			DisableWebPagePreview: true,
		}
		return json.Marshal(body)
	case "discord":
		body := struct {
			Content string `json:"content"`
		}{Content: truncateRunes(ev.Message, discordTextLimit)}
		return json.Marshal(body)
	default:
		return nil, fmt.Errorf("webhook %s: unknown preset %q", e.id, e.cfg.Preset)
	}
}

// deliver POSTs the event to the endpoint, retrying transient failures with
// exponential backoff: 2xx is success, 429/5xx and transport errors are
// retried up to RetryMax times, any other 4xx fails immediately. It returns
// the exact body that was (attempted to be) sent so a failed delivery can be
// parked byte-for-byte.
func (e *endpoint) deliver(ctx context.Context, ev Event) ([]byte, error) {
	payload, err := e.buildPayload(ev)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= e.cfg.RetryMax; attempt++ {
		if attempt > 0 {
			if !sleepContext(ctx, e.backoff(attempt)) {
				return nil, ctx.Err()
			}
		}
		err := e.attempt(ctx, payload)
		if err == nil {
			return payload, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return payload, err
		}
	}
	return payload, lastErr
}

// park stores a failed delivery's exact payload in the outbox for later
// redelivery. A nil outbox keeps the failure in memory only (it is already
// logged by the caller); a nil payload means there is nothing to replay.
func (e *endpoint) park(_ context.Context, payload []byte, outbox Outbox) error {
	if outbox == nil || payload == nil {
		return nil
	}
	return outbox.Add(e.id, payload)
}

// permanentError marks a failure that must not be retried.
type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// attempt performs exactly one HTTP POST of the payload.
func (e *endpoint) attempt(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return permanentError{fmt.Errorf("webhook %s: build request: %w", e.id, err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.Preset == "raw" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("X-hhauto-Timestamp", ts)
		req.Header.Set("X-hhauto-Signature", "sha256="+sign(e.secret, ts, payload))
	}

	resp, err := e.client.Do(req)
	if err != nil { // transport errors are transient by definition
		return fmt.Errorf("webhook %s: POST: %w", e.id, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return fmt.Errorf("webhook %s: HTTP %d", e.id, resp.StatusCode)
	default:
		return permanentError{fmt.Errorf("webhook %s: HTTP %d", e.id, resp.StatusCode)}
	}
}

// isRetryable reports whether a delivery error should be retried: everything
// except explicit permanent (non-429 4xx) failures.
func isRetryable(err error) bool {
	var p permanentError
	return !errors.As(err, &p)
}

// sign computes hex(HMAC-SHA256(secret, "v1." + timestamp + "." + body)).
func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v1." + timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// truncateRunes cuts s to at most limit characters (Unicode code points),
// matching how Telegram and Discord count message length.
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

// sleepContext waits for d or until ctx is done; reports whether the full
// delay elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
