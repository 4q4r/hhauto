package captcha

import (
	"context"
	"fmt"
	"time"
)

// Event kinds reported through the chain audit callback. Wave 3 persists
// these as captcha_events rows; the payload is intentionally flat.
const (
	EventSolve   = "solve"
	EventBalance = "balance"
)

// Event is one audit observation about a provider attempt.
// (Unexported package prefix avoids the stutter captcha.Event.)
// LatencyMS is the wall-clock duration of the attempt in milliseconds.
type Event struct {
	ProviderID string
	Kind       string
	OK         bool
	LatencyMS  int64
}

type chainSolver struct {
	providers []Solver
	onEvent   func(Event)
}

// NewChain returns a Chain that solves through providers in the given order.
// A provider that returns an error — including a context timeout or
// cancellation while solving — is skipped and the next one is tried. When
// every provider fails, the returned error wraps ErrAllFailed. An empty
// provider list yields ErrNoProviders on Solve.
//
// onEvent, when non-nil, is called exactly once per provider attempt with
// the measured latency; a nil callback is allowed and disables auditing.
func NewChain(providers []Solver, onEvent func(Event)) Chain {
	return &chainSolver{providers: providers, onEvent: onEvent}
}

func (c *chainSolver) Solve(ctx context.Context, ch Challenge) (Solution, error) {
	if len(c.providers) == 0 {
		return Solution{}, ErrNoProviders
	}

	var lastErr error
	for _, p := range c.providers {
		start := time.Now()
		sol, err := p.Solve(ctx, ch)
		c.report(Event{
			ProviderID: p.ID(),
			Kind:       EventSolve,
			OK:         err == nil,
			LatencyMS:  time.Since(start).Milliseconds(),
		})
		if err == nil {
			if sol.ProviderID == "" {
				sol.ProviderID = p.ID()
			}
			return sol, nil
		}
		// Any failure — protocol error, timeout, or cancelled context —
		// skips this provider; the caller's context propagates naturally
		// to the remaining ones.
		lastErr = err
	}
	return Solution{}, fmt.Errorf("%w: tried %d provider(s), last error: %w", ErrAllFailed, len(c.providers), lastErr)
}

func (c *chainSolver) report(e Event) {
	if c.onEvent != nil {
		c.onEvent(e)
	}
}
