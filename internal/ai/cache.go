package ai

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// CacheStore is the minimal verdict cache contract. The Runner uses it to
// skip identical (task, prompt, model, params, input) requests within the
// task TTL; Wave 3 wires a SQLite-backed adapter, tests may pass nil or the
// in-memory implementation below. Best-effort by design: cache errors are
// logged and degrade to a miss, never to a failed run.
type CacheStore interface {
	Get(ctx context.Context, key string) (verdict json.RawMessage, ok bool, err error)
	Set(ctx context.Context, key string, verdict json.RawMessage, ttl time.Duration) error
}

type memoryEntry struct {
	verdict   json.RawMessage
	expiresAt time.Time
}

// MemoryCache is a process-local TTL cache (safe for concurrent use). It is
// the reference CacheStore and covers tests and single-process deployments;
// a SQLite adapter can implement the same interface for durability.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
	now     func() time.Time
}

// NewMemoryCache builds an empty cache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: map[string]memoryEntry{}, now: time.Now}
}

// Get returns the cached verdict for key when present and unexpired; an
// expired entry is dropped lazily. A miss is (nil, false, nil).
func (c *MemoryCache) Get(_ context.Context, key string) (json.RawMessage, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	if c.now().After(e.expiresAt) {
		delete(c.entries, key)
		return nil, false, nil
	}
	return e.verdict, true, nil
}

// Set stores verdict under key for ttl; ttl <= 0 is a no-op.
func (c *MemoryCache) Set(_ context.Context, key string, verdict json.RawMessage, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]memoryEntry{}
	}
	c.entries[key] = memoryEntry{verdict: verdict, expiresAt: c.now().Add(ttl)}
	return nil
}
