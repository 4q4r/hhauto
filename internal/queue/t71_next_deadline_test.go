package queue

import (
	"context"
	"testing"
	"time"
)

// T71: the daemon's maintenance wake-up reads the earliest non-zero
// Deadline queued for an account. Zero-deadline tasks (the seeded run
// tasks) never wake it.
func TestNextDeadline(t *testing.T) {
	q := New()
	ctx := context.Background()

	if q.Len("a") != 0 {
		t.Fatal("fresh queue must be empty")
	}
	if dl, ok := q.NextDeadline("a"); ok {
		t.Fatalf("NextDeadline on an unknown account = %v, want !ok", dl)
	}

	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := q.Enqueue(ctx, Task{AccountID: "a", Kind: KindApply, DedupKey: "run", Priority: PrioApply}); err != nil {
		t.Fatalf("enqueue run task: %v", err)
	}
	if dl, ok := q.NextDeadline("a"); ok {
		t.Fatalf("NextDeadline with only zero-deadline tasks = %v, want !ok", dl)
	}

	d1 := t0.Add(4 * time.Hour)
	if err := q.Enqueue(ctx, Task{AccountID: "a", Kind: KindLift, DedupKey: "lift:w1", Priority: PrioLift, Deadline: d1}); err != nil {
		t.Fatalf("enqueue lift: %v", err)
	}
	d2 := t0.Add(2 * time.Hour)
	if err := q.Enqueue(ctx, Task{AccountID: "a", Kind: KindLift, DedupKey: "lift:w0", Priority: PrioLift, Deadline: d2}); err != nil {
		t.Fatalf("enqueue lift2: %v", err)
	}
	dl, ok := q.NextDeadline("a")
	if !ok || !dl.Equal(d2) {
		t.Fatalf("NextDeadline = %v ok=%v, want %v", dl, ok, d2)
	}

	// Dequeuing the earliest task moves the deadline forward.
	if got, ok := q.Dequeue(ctx, "a"); !ok || got.DedupKey != "run" {
		t.Fatalf("dequeue = %+v ok=%v, want the priority-3 run task", got, ok)
	}
	// The run task is gone; the earliest lift task is d2 (priority equal, seq order).
	dl, ok = q.NextDeadline("a")
	if !ok || !dl.Equal(d2) {
		t.Fatalf("NextDeadline after dequeue = %v ok=%v, want %v", dl, ok, d2)
	}
}
