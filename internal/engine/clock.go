package engine

import (
	"context"
	"time"
)

// Clock is the injected time source for the engine loops (scheduler tick,
// outbox tick, pause sleeps). Tests drive a fake clock deterministically.
type Clock interface {
	Now() time.Time
	// After returns a channel that fires once after d (time.After for the
	// real clock).
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

// RealClock is the production Clock.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// sleepClock waits d (or until ctx is done) on the given clock; reports
// whether the full delay elapsed.
func sleepClock(ctx context.Context, c Clock, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-c.After(d):
		return true
	}
}
