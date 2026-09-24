package hhapi

import (
	"context"
	"math"
	"time"

	"golang.org/x/time/rate"
)

// defaultRPS matches the config default: half a request per second per
// account. A zero/negative cfg.RPS must never disable limiting — the whole
// point of the limiter is to keep the account away from burst fingerprints.
const defaultRPS = 0.5

// limiter serializes every outbound HTTP attempt through a token bucket with
// burst 1: strictly one request at a time, spaced at least 1/RPS apart.
// Human-like inter-mutation jitter is the workers' job, not the client's.
type limiter struct{ l *rate.Limiter }

func newLimiter(rps float64) *limiter {
	if rps <= 0 {
		rps = defaultRPS
	}
	return &limiter{l: rate.NewLimiter(rate.Limit(rps), 1)}
}

func (l *limiter) wait(ctx context.Context) error { return l.l.Wait(ctx) }

// limit reports the effective requests-per-second (test/observability).
func (l *limiter) limit() float64 { return float64(l.l.Limit()) }

// reserveFor reports the wait a caller would face right now (test helper).
func (l *limiter) reserveFor() time.Duration {
	r := l.l.Reserve()
	d := r.Delay()
	r.Cancel()
	return time.Duration(math.Round(float64(d)))
}
