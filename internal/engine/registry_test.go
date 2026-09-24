package engine

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// Latent T13-adapter bug pinned in the engine: a Spec literal without a
// weekday mask (Days == 0) must normalize to scheduler.All at registration,
// so materialized rows and next_run recomputation fire every day instead of
// never (Spec.Next skips Days == 0 masks entirely).
func TestRegisterScheduleNormalizesEmptyDayMask(t *testing.T) {
	t.Cleanup(resetRegistrations)

	err := RegisterSchedule("days.normalize", scheduler.Spec{Times: []string{"09:00"}},
		func(context.Context, string) (queue.Task, error) { return queue.Task{}, nil })
	if err != nil {
		t.Fatalf("RegisterSchedule: %v", err)
	}

	b := registeredSchedules()["days.normalize"]
	if b.spec.Days != scheduler.All {
		t.Fatalf("spec.Days = %d, want scheduler.All (%d)", b.spec.Days, scheduler.All)
	}
	next := b.spec.Next(time.Now(), rand.New(rand.NewSource(1)))
	if next.IsZero() {
		t.Fatal("normalized spec must produce a non-zero next run")
	}
}
