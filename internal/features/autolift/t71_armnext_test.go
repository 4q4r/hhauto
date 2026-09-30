package autolift

import (
	"context"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// T71: ArmNext re-arms the 4h lift chain for every enabled account. It is
// the daemon wake-up's recovery point: seeding is dedup-idempotent, so the
// daemon can call it on every loop iteration and the chain never dies.
func TestArmNextSeedsAndReArms(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)
	q := queue.New()
	if err := f.RegisterTasks(context.Background(), q, scheduler.NewRegistry()); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if q.Len("acc1") != 1 {
		t.Fatalf("queued = %d, want 1 after RegisterTasks", q.Len("acc1"))
	}

	// ArmNext while the window task is queued: dedup no-op, no double seed.
	if err := f.ArmNext(context.Background()); err != nil {
		t.Fatalf("ArmNext: %v", err)
	}
	if q.Len("acc1") != 1 {
		t.Fatalf("queued = %d, want 1 (dedup)", q.Len("acc1"))
	}

	// Consume the seeded task; ArmNext re-arms the chain with a fresh
	// deadline ~4h ahead (the server_timer cadence).
	if _, ok := q.Dequeue(context.Background(), "acc1"); !ok {
		t.Fatalf("dequeue failed")
	}
	if err := f.ArmNext(context.Background()); err != nil {
		t.Fatalf("ArmNext after drain: %v", err)
	}
	if q.Len("acc1") != 1 {
		t.Fatalf("queued = %d, want 1 after re-arm", q.Len("acc1"))
	}
	task, ok := q.Dequeue(context.Background(), "acc1")
	if !ok {
		t.Fatalf("dequeue re-armed task failed")
	}
	if task.Kind != queue.KindLift || task.Deadline.IsZero() {
		t.Fatalf("re-armed task = %+v, want KindLift with a deadline", task)
	}
	if !task.Deadline.After(now) {
		t.Fatalf("re-armed deadline %v, want after the feature clock %v (~+4h)", task.Deadline, now)
	}
}

// ArmNext on a disabled feature is a no-op (the daemon arms unconditionally).
func TestArmNextDisabledNoop(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.AutoLift.Enabled = false })
	f := h.feature(t)
	q := queue.New()
	if err := f.ArmNext(context.Background()); err != nil {
		t.Fatalf("ArmNext: %v", err)
	}
	if q.Len("acc1") != 0 {
		t.Fatalf("queued = %d, want 0 (disabled)", q.Len("acc1"))
	}
}
