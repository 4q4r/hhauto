package autolift

// T114: a 409 on touch (hh 4h server timer not elapsed yet) is the
// ErrTouchTooEarly sentinel. The lift plan answers with a short too-early
// cooldown (config features.auto_lift.too_early_cooldown, default 30m)
// instead of the 4h re-count, and retries when the cooldown elapses.

import (
	"context"
	"testing"
	"time"

	"hhauto/internal/config"
)

// Full too-early cycle: the discover pass hits 409 → plan = now+30m; a
// planned retry that is still too early re-plans another 30m; the first
// accepted touch re-counts 4h.
func TestTouchTooEarlyCooldownCycle(t *testing.T) {
	h := newHarness(t, nil, liftable("hash1", now.Add(-5*time.Hour)))
	h.fake.resumes[0].NextTouch = now.Add(-30 * time.Minute)
	var calls []armed
	f, setTime := stFeature(t, h, &captureQueue{}, &calls)

	// Pass 1 (discover): 409 → cooldown from now.
	h.fake.touchErr = func(int) error { return &touchError{status: 409} }
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount pass 1: %v", err)
	}
	if h.fake.touch != 1 {
		t.Fatalf("touch calls = %d, want 1", h.fake.touch)
	}
	if got := lastArmed(calls); got.d != 30*time.Minute {
		t.Fatalf("armed delay after 409 = %v, want 30m cooldown", got.d)
	}

	// Pass 2 (planned): still too early → another 30m from the new now.
	setTime(now.Add(30 * time.Minute))
	calls = nil
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount pass 2: %v", err)
	}
	if h.fake.touch != 2 {
		t.Fatalf("touch calls = %d, want 2 (cooldown retry fired)", h.fake.touch)
	}
	if got := lastArmed(calls); got.d != 30*time.Minute {
		t.Fatalf("armed delay after second 409 = %v, want 30m cooldown", got.d)
	}

	// Pass 3 (planned): touch accepted → plain 4h re-count.
	setTime(now.Add(60 * time.Minute))
	h.fake.touchErr = nil
	calls = nil
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount pass 3: %v", err)
	}
	if got := lastArmed(calls); got.d != timer4h {
		t.Fatalf("armed delay after accepted touch = %v, want 4h", got.d)
	}
}

// The cooldown interval is configurable ([features.auto_lift]
// too_early_cooldown).
func TestTouchTooEarlyCooldownFromConfig(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoLift.TooEarlyCooldown = "45m"
	}, liftable("hash1", now.Add(-5*time.Hour)))
	h.fake.resumes[0].NextTouch = now.Add(-30 * time.Minute)
	var calls []armed
	f, _ := stFeature(t, h, &captureQueue{}, &calls)

	h.fake.touchErr = func(int) error { return &touchError{status: 409} }
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if got := lastArmed(calls); got.d != 45*time.Minute {
		t.Fatalf("armed delay after 409 = %v, want 45m (configured cooldown)", got.d)
	}
}
