package autolift

// Wave 3 review fix tests (fix/t10-wave3): jitter sampled [min,max] outside
// the scheduler spec, and even schedule spread for non-divisor PerDay.

import (
	"math/rand"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// FIX 4: jitter "20-40m" is sampled in [min,max] by the feature (one-shot
// on the seeded task deadline); Spec.Jitter stays 0 so the engine never
// re-jitters on top.
func TestJitterSampledRangeNotSpecLevel(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoLift.Jitter = "20-40m"
	}, fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)
	f.opt.Rand = rand.New(rand.NewSource(7)) // deterministic sampling

	q := queue.New()
	reg := scheduler.NewRegistry()
	if err := f.RegisterTasks(ctxbg(t), q, reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}

	spec, _ := reg.Get("features.autolift")
	if spec.Jitter != 0 {
		t.Fatalf("spec.Jitter = %v, want 0 (feature samples its own jitter)", spec.Jitter)
	}
	task, ok := q.Dequeue(ctxbg(t), "acc1")
	if !ok {
		t.Fatalf("no seeded task")
	}
	d := task.Deadline.Sub(now)
	if d <= timer4h+20*time.Minute || d > timer4h+40*time.Minute {
		t.Fatalf("seeded deadline = %v after now, want in (+4h20m, +4h40m]", d)
	}
}

// FIX 5: PerDay values that do not divide 24 spread evenly (no tail hole).
func TestScheduleTimesNonDivisorPerDay(t *testing.T) {
	got := scheduleTimes(5)
	want := []string{"00:00", "04:48", "09:36", "14:24", "19:12"}
	if len(got) != len(want) {
		t.Fatalf("times(5) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("times(5)[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	got7 := scheduleTimes(7)
	if len(got7) != 7 {
		t.Fatalf("times(7) = %v, want 7 slots", got7)
	}
	if got7[0] != "00:00" || got7[len(got7)-1] >= "24:00" {
		t.Fatalf("times(7) = %v, must start at 00:00 and stay within the day", got7)
	}
	// divisors keep their exact spread
	if got := scheduleTimes(4); got[1] != "06:00" || got[3] != "18:00" {
		t.Fatalf("times(4) = %v", got)
	}
}
