package hhapi

import (
	"context"
	"math/rand/v2"
	"time"
)

// maxAttempts bounds GET and idempotent-POST retries (3 total tries).
const maxAttempts = 3

// retryBackoffBase and retryBackoffCap shape the exponential curve:
// 500ms * 2^attempt, capped at 8s, with full jitter on top.
const (
	retryBackoffBase = 500 * time.Millisecond
	retryBackoffCap  = 8 * time.Second
)

// fullJitter returns a uniform random duration in [0, min(cap, base*2^attempt)].
func fullJitter(attempt int, r *rand.Rand) time.Duration {
	backoff := retryBackoffBase << attempt
	if backoff > retryBackoffCap || backoff <= 0 {
		backoff = retryBackoffCap
	}
	return time.Duration(r.Int64N(int64(backoff)))
}

// retryableStatus reports whether an HTTP status warrants another attempt
// for idempotent requests: 429 and 5xx except 501 (not implemented is
// permanent).
func retryableStatus(code int) bool {
	if code == 429 {
		return true
	}
	return code >= 500 && code <= 599 && code != 501
}

// sleepCtx sleeps d or returns early with the context error.
func sleepCtx(ctx context.Context, d time.Duration) error {
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
