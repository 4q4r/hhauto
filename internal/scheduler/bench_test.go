package scheduler

// Benchmark for Spec.Next — the per-day window+mask+jitter computation the
// engine's scheduler loop runs for every registered schedule on every tick.
// The spec carries three times (window), a workday mask (mask skips
// weekends during the search) and a 40-minute jitter (one rand draw).

import (
	"math/rand"
	"testing"
	"time"
)

func BenchmarkNext(b *testing.B) {
	// Mon..Fri mask: the search from a Thursday morning must match the
	// mask bit AND pick the MIN candidate among the remaining times (R5).
	spec, err := Parse("Europe/Moscow", []string{"09:00", "13:30", "22:00"}, []string{"mon", "tue", "wed", "thu", "fri"})
	if err != nil {
		b.Fatal(err)
	}
	spec.Jitter = 40 * time.Minute
	rnd := rand.New(rand.NewSource(42))
	after := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC) // Thursday 10:00 MSK

	b.ReportAllocs()
	for b.Loop() {
		got := spec.Next(after, rnd)
		if !got.After(after) {
			b.Fatalf("Next = %v, want strictly after %v", got, after)
		}
	}
}
