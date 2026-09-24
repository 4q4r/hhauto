package hhapi

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"testing"
	"time"
)

func TestFullJitterBounds(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 7))
	caps := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	for attempt, wantCap := range caps {
		for range 50 {
			d := fullJitter(attempt, r)
			if d < 0 || d > wantCap {
				t.Fatalf("fullJitter(%d) = %v, want in [0, %v]", attempt, d, wantCap)
			}
		}
	}
	// Attempt far beyond growth is capped at 8s.
	for range 50 {
		if d := fullJitter(20, r); d > 8*time.Second {
			t.Fatalf("fullJitter(20) = %v, want <= 8s cap", d)
		}
	}
}

func TestRetryableStatus(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		if !retryableStatus(code) {
			t.Fatalf("retryableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range []int{200, 301, 400, 401, 403, 404, 422, 501} {
		if retryableStatus(code) {
			t.Fatalf("retryableStatus(%d) = true, want false", code)
		}
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepCtx = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx(canceled) = %v, want context.Canceled", err)
	}
}

func TestNetworkClassHelpers(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Err: errors.New("refused")}
	readErr := &net.OpError{Op: "read", Err: errors.New("reset")}
	if !isDialError(dialErr) || isDialError(readErr) {
		t.Fatal("isDialError misclassifies net.OpError ops")
	}
}
