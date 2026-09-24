package featurekit

import (
	"math/rand"
	"testing"
	"time"
)

func TestParseJitterRange(t *testing.T) {
	cases := []struct {
		in      string
		wantMin time.Duration
		wantMax time.Duration
		wantErr bool
	}{
		{"", 0, 0, false},
		{"40m", 40 * time.Minute, 40 * time.Minute, false},
		{"20-40m", 20 * time.Minute, 40 * time.Minute, false},
		{"90s", 90 * time.Second, 90 * time.Second, false},
		{"2h", 2 * time.Hour, 2 * time.Hour, false},
		{"bogus", 0, 0, true},
		{"40-bogus", 0, 0, true},
		{"-5m", 0, 0, true},
	}
	for _, tc := range cases {
		mn, mx, err := ParseJitterRange(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseJitterRange(%q) err = nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseJitterRange(%q): %v", tc.in, err)
		}
		if mn != tc.wantMin || mx != tc.wantMax {
			t.Fatalf("ParseJitterRange(%q) = %v,%v want %v,%v", tc.in, mn, mx, tc.wantMin, tc.wantMax)
		}
	}
}

func TestJitterDuration(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	// max-only jitter: uniform [0, max]
	for range 50 {
		d := JitterDuration(0, time.Minute, rnd)
		if d < 0 || d > time.Minute {
			t.Fatalf("jitter %v outside [0,1m]", d)
		}
	}
	// range jitter: uniform [min, max]
	for range 50 {
		d := JitterDuration(time.Minute, 2*time.Minute, rnd)
		if d < time.Minute || d > 2*time.Minute {
			t.Fatalf("jitter %v outside [1m,2m]", d)
		}
	}
	// nil rand / degenerate range is safe
	if d := JitterDuration(0, 0, nil); d != 0 {
		t.Fatalf("degenerate jitter = %v, want 0", d)
	}
}
