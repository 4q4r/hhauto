package engine

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"hhauto/internal/config"
)

func pacingCfg(mode, window, jitter string, minDelaySec int, night *bool) config.Pacing {
	return config.Pacing{
		Mode:         mode,
		ActiveWindow: window,
		Jitter:       jitter,
		MinDelaySec:  minDelaySec,
		NightPause:   night,
	}
}

func boolPtr(b bool) *bool { return &b }

func TestParseDurationRange(t *testing.T) {
	cases := []struct {
		in      string
		lo, hi  time.Duration
		wantErr bool
	}{
		{"20-40m", 20 * time.Minute, 40 * time.Minute, false},
		{"40m", 40 * time.Minute, 40 * time.Minute, false},
		{"90s", 90 * time.Second, 90 * time.Second, false},
		{"1h-2h", time.Hour, 2 * time.Hour, false},
		{"30s-5m", 30 * time.Second, 5 * time.Minute, false},
		{"0-5m", 0, 5 * time.Minute, false},
		{"", 0, 0, true},
		{"abc", 0, 0, true},
		{"40-", 0, 0, true},
		{"-20m", 0, 0, true},
		{"20-", 0, 0, true},
		{"1-2", 0, 0, true},
		{"20-40", 0, 0, true},
		{"20-1h30m", 0, 0, true}, // multi-unit right bound: refuse, never guess
	}
	for _, tc := range cases {
		lo, hi, err := ParseDurationRange(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDurationRange(%q): expected error, got (%v, %v)", tc.in, lo, hi)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDurationRange(%q): unexpected error %v", tc.in, err)
			continue
		}
		if lo != tc.lo || hi != tc.hi {
			t.Errorf("ParseDurationRange(%q) = (%v, %v), want (%v, %v)", tc.in, lo, hi, tc.lo, tc.hi)
		}
	}
}

func TestPacerUniform(t *testing.T) {
	p := NewPacer(pacingCfg("uniform", "08:00-22:00", "", 90, boolPtr(true)), nil)
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{base, base.Add(time.Hour)} {
		p.now = func() time.Time { return at }
		if d := p.nextDelay("a1"); d != 90*time.Second {
			t.Fatalf("uniform call %d: delay = %v, want 90s", i, d)
		}
	}
}

func TestPacerBurst(t *testing.T) {
	p := NewPacer(pacingCfg("burst", "08:00-22:00", "", 90, boolPtr(true)), nil)
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return base }
	if d := p.nextDelay("a1"); d != 0 {
		t.Fatalf("burst: delay = %v, want 0", d)
	}
}

func TestPacerUnknownModeFallsBackToAdaptive(t *testing.T) {
	p := NewPacer(pacingCfg("weird", "08:00-22:00", "", 90, boolPtr(true)), nil)
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return base }
	p.SetExpectedDaily("a1", 200)
	// 14h window / 200 expected = 252s spacing between mutations.
	want := (14 * time.Hour) / 200
	if d := p.nextDelay("a1"); d != 0 {
		t.Fatalf("adaptive fallback first: delay = %v, want 0", d)
	}
	if d := p.nextDelay("a1"); d != want {
		t.Fatalf("adaptive fallback spacing = %v, want %v", d, want)
	}
}

func TestPacerAdaptiveSpacing(t *testing.T) {
	p := NewPacer(pacingCfg("adaptive", "08:00-22:00", "", 0, boolPtr(true)), nil)
	p.SetExpectedDaily("a1", 200)
	want := (14 * time.Hour) / 200 // 4m12s

	base := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	cur := base
	p.now = func() time.Time { return cur }

	if d := p.nextDelay("a1"); d != 0 {
		t.Fatalf("adaptive first mutation: delay = %v, want 0 (no prior activity)", d)
	}
	// Halfway through the FIRST interval the remainder is left.
	cur = base.Add(want / 2)
	if d := p.nextDelay("a1"); d != want/2 {
		t.Fatalf("adaptive halfway: delay = %v, want %v", d, want/2)
	}
	// The slot comes due one full interval after the halfway grant started
	// waiting (fixed-rate spacing anchored at grant time).
	cur = base.Add(2 * want)
	if d := p.nextDelay("a1"); d != 0 {
		t.Fatalf("adaptive slot due: delay = %v, want 0", d)
	}
	// A long idle period must not burst-catch-up: after idle the next
	// mutation goes immediately and the one after waits a full interval.
	cur = base.Add(6 * time.Hour)
	if d := p.nextDelay("a1"); d != 0 {
		t.Fatalf("adaptive after idle: delay = %v, want 0", d)
	}
	if d := p.nextDelay("a1"); d != want {
		t.Fatalf("adaptive after idle second: delay = %v, want %v", d, want)
	}
}

func TestPacerAdaptivePerAccount(t *testing.T) {
	p := NewPacer(pacingCfg("adaptive", "08:00-22:00", "", 0, boolPtr(true)), nil)
	p.SetExpectedDaily("a1", 200)
	p.SetExpectedDaily("a2", 100)
	want1 := (14 * time.Hour) / 200
	want2 := (14 * time.Hour) / 100
	base := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return base }
	if d := p.nextDelay("a1"); d != 0 {
		t.Fatalf("a1 first: %v", d)
	}
	if d := p.nextDelay("a2"); d != 0 {
		t.Fatalf("a2 first: %v", d)
	}
	if d := p.nextDelay("a1"); d != want1 {
		t.Fatalf("a1 spacing = %v, want %v", d, want1)
	}
	if d := p.nextDelay("a2"); d != want2 {
		t.Fatalf("a2 spacing = %v, want %v", d, want2)
	}
}

func TestPacerExpectedDailyDefault(t *testing.T) {
	p := NewPacer(pacingCfg("adaptive", "08:00-22:00", "", 0, boolPtr(true)), nil)
	base := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return base }
	p.nextDelay("unset") // grant first slot
	// Default expected daily = 200 (config default daily_apply_cap).
	if d := p.nextDelay("unset"); d != (14*time.Hour)/200 {
		t.Fatalf("default expected daily: delay = %v, want %v", d, (14*time.Hour)/200)
	}
}

func TestPacerJitter(t *testing.T) {
	// min == max jitter is deterministic: base + 10m.
	p := NewPacer(pacingCfg("uniform", "08:00-22:00", "10m-10m", 90, boolPtr(true)), nil)
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return base }
	if d := p.nextDelay("a1"); d != 90*time.Second+10*time.Minute {
		t.Fatalf("jittered delay = %v, want %v", d, 90*time.Second+10*time.Minute)
	}
}

func TestPacerInvalidJitterIgnored(t *testing.T) {
	p := NewPacer(pacingCfg("uniform", "08:00-22:00", "garbage", 90, boolPtr(true)), nil)
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return base }
	if d := p.nextDelay("a1"); d != 90*time.Second {
		t.Fatalf("invalid jitter must be ignored: delay = %v, want 90s", d)
	}
}

func TestPacerNightPause(t *testing.T) {
	p := NewPacer(pacingCfg("uniform", "08:00-22:00", "", 90, boolPtr(true)), nil)

	// Inside the window: normal delay.
	cur := time.Date(2026, 9, 24, 21, 59, 0, 0, time.UTC)
	p.now = func() time.Time { return cur }
	if d := p.nextDelay("a1"); d != 90*time.Second {
		t.Fatalf("inside window: delay = %v, want 90s", d)
	}

	// Outside the window: pause until the next window start (08:00 next day).
	cur = time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC).Sub(cur)
	if d := p.nextDelay("a1"); d != want {
		t.Fatalf("night pause: delay = %v, want %v", d, want)
	}

	// Before the window opens the same day: wait until 08:00.
	cur = time.Date(2026, 9, 24, 6, 30, 0, 0, time.UTC)
	want = time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC).Sub(cur)
	if d := p.nextDelay("a1"); d != want {
		t.Fatalf("early morning pause: delay = %v, want %v", d, want)
	}
}

func TestPacerNightPauseDisabled(t *testing.T) {
	p := NewPacer(pacingCfg("uniform", "08:00-22:00", "", 90, boolPtr(false)), nil)
	cur := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return cur }
	if d := p.nextDelay("a1"); d != 90*time.Second {
		t.Fatalf("night pause disabled: delay = %v, want 90s", d)
	}
}

func TestPacerWindowCrossingMidnight(t *testing.T) {
	// 22:00-06:00: active at 23:00, paused at 12:00 (until 22:00).
	p := NewPacer(pacingCfg("uniform", "22:00-06:00", "", 30, boolPtr(true)), nil)
	cur := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return cur }
	if d := p.nextDelay("a1"); d != 30*time.Second {
		t.Fatalf("midnight window active: delay = %v, want 30s", d)
	}
	cur = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 24, 22, 0, 0, 0, time.UTC).Sub(cur)
	if d := p.nextDelay("a1"); d != want {
		t.Fatalf("midnight window paused: delay = %v, want %v", d, want)
	}
}

func TestPacerInvalidWindowMeansAlwaysActive(t *testing.T) {
	p := NewPacer(pacingCfg("uniform", "not-a-window", "", 30, boolPtr(true)), nil)
	cur := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return cur }
	if d := p.nextDelay("a1"); d != 30*time.Second {
		t.Fatalf("invalid window: delay = %v, want 30s (always active)", d)
	}
}

func TestPacerWaitRespectsContext(t *testing.T) {
	p := NewPacer(pacingCfg("uniform", "08:00-22:00", "", 1, boolPtr(true)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := p.Wait(ctx, "a1")
	if err == nil {
		t.Fatalf("Wait: expected context error, got nil")
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("Wait did not respect ctx cancellation (took %v)", time.Since(start))
	}
}

func TestPacerWaitConcurrentAccountsRaceFree(t *testing.T) {
	// Deterministic shared Rand across goroutines: all rnd access must be
	// mutex-guarded (math/rand.Rand is not goroutine-safe).
	p := NewPacer(pacingCfg("adaptive", "08:00-22:00", "1m-2m", 0, boolPtr(true)), rand.New(rand.NewSource(42)))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		id := "acc"
		go func() {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_ = p.Wait(ctx, id) // bursts away at zero spacing; jitter wait cut by ctx
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
