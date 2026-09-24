package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
	"hhauto/internal/webhook"
)

// Redeliverer replays parked outbox payloads.
//
// Design note (documented choice per T12 scope): webhook.Notifier only
// accepts structured Events and re-serializes them, which cannot replay a
// parked payload byte-for-byte (telegram/discord bodies are preset-shaped,
// raw bodies carry a signature over the exact bytes). The webhook package
// exposes no DeliverPayload-style export, so the engine owns this small
// delivery client that mirrors webhook/endpoint.go's wire contract: same
// headers, HMAC re-computed at redelivery time, per-endpoint proxy, 2xx =
// success, everything else = retry later.
type Redeliverer interface {
	Redeliver(ctx context.Context, item webhook.OutboxItem) error
}

type redeliverTarget struct {
	url    string
	preset string // raw | telegram | discord
	secret string // raw preset only
	client *http.Client
}

type redeliverer struct {
	targets map[string]*redeliverTarget // endpoint URL → target
}

// newRedeliverer mirrors webhook.New's tolerance: an endpoint with a bad
// configuration is disabled with an error log instead of failing startup.
func newRedeliverer(endpoints []config.Webhook, reg proxy.Registry, log *slog.Logger) *redeliverer {
	r := &redeliverer{targets: map[string]*redeliverTarget{}}
	for _, cfg := range endpoints {
		t, err := newRedeliverTarget(cfg, reg)
		if err != nil {
			log.Error("outbox redelivery endpoint disabled: configuration error", "err", err)
			continue
		}
		r.targets[cfg.URL] = t
	}
	return r
}

func newRedeliverTarget(cfg config.Webhook, reg proxy.Registry) (*redeliverTarget, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("outbox redelivery: url is empty")
	}
	switch cfg.Preset {
	case "raw", "telegram", "discord":
	default:
		return nil, fmt.Errorf("outbox redelivery %s: unknown preset %q", cfg.URL, cfg.Preset)
	}
	t := &redeliverTarget{url: cfg.URL, preset: cfg.Preset}
	if cfg.Preset == "raw" {
		if cfg.SecretEnv == "" {
			return nil, fmt.Errorf("outbox redelivery %s: secret_env is required for the raw preset", cfg.URL)
		}
		secret, ok := os.LookupEnv(cfg.SecretEnv)
		if !ok {
			return nil, fmt.Errorf("outbox redelivery %s: secret env %q is not set", cfg.URL, cfg.SecretEnv)
		}
		t.secret = secret
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if cfg.ProxyRef != "" {
		if reg == nil {
			return nil, fmt.Errorf("outbox redelivery %s: proxy_ref set but no proxy registry", cfg.URL)
		}
		d, err := reg.Dialer(cfg.ProxyRef)
		if err != nil {
			return nil, fmt.Errorf("outbox redelivery %s: %w", cfg.URL, err)
		}
		client.Transport = &http.Transport{DialContext: d.DialContext}
	}
	t.client = client
	return t, nil
}

// Redeliver POSTs the exact parked payload. The raw preset's HMAC headers
// are recomputed over the stored bytes now (the signature covers a
// timestamp, so the parked headers — if any — are stale by design).
func (r *redeliverer) Redeliver(ctx context.Context, item webhook.OutboxItem) error {
	t, ok := r.targets[item.EndpointID]
	if !ok {
		return fmt.Errorf("outbox redelivery: unknown endpoint %q", item.EndpointID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(item.Payload))
	if err != nil {
		return fmt.Errorf("outbox redelivery %s: %w", t.url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.preset == "raw" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("X-hhauto-Timestamp", ts)
		req.Header.Set("X-hhauto-Signature", "sha256="+redeliverSign(t.secret, ts, item.Payload))
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("outbox redelivery %s: POST: %w", t.url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("outbox redelivery %s: HTTP %d", t.url, resp.StatusCode)
}

// redeliverSign matches webhook/endpoint.go: hex(HMAC-SHA256(secret,
// "v1." + timestamp + "." + body)).
func redeliverSign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v1." + timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// redeliveryBook tracks per-item redelivery attempt counters (in-memory;
// a process restart resets them, which only costs one backoff step).
type redeliveryBook struct {
	mu       sync.Mutex
	attempts map[int64]int
}

func (b *redeliveryBook) next(id int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts == nil {
		b.attempts = map[int64]int{}
	}
	b.attempts[id]++
	return b.attempts[id]
}

// clear resets an item's counter — called on every successful delivery so
// the next failure starts from the base backoff again.
func (b *redeliveryBook) clear(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.attempts, id)
}
