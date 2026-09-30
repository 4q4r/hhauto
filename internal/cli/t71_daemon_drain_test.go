package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"hhauto/internal/queue"
)

// T71 maintenance drain: DUE tasks (non-zero elapsed deadline) dispatch to
// their handlers; future and zero-deadline tasks park back untouched.
func TestDrainDueTasksRunsDueAndParksRest(t *testing.T) {
	q := queue.New()
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)

	if err := q.Enqueue(ctx, queue.Task{AccountID: "a", Kind: queue.KindLift, DedupKey: "due", Deadline: past}); err != nil {
		t.Fatalf("enqueue due: %v", err)
	}
	if err := q.Enqueue(ctx, queue.Task{AccountID: "a", Kind: queue.KindLift, DedupKey: "future", Deadline: future}); err != nil {
		t.Fatalf("enqueue future: %v", err)
	}
	if err := q.Enqueue(ctx, queue.Task{AccountID: "a", Kind: queue.KindApply, DedupKey: "nodeadline"}); err != nil {
		t.Fatalf("enqueue nodeadline: %v", err)
	}

	var handled []string
	m := maintenanceDeps{
		Queue: q,
		Handlers: map[queue.TaskKind]maintenanceHandler{
			queue.KindLift: func(_ context.Context, _ string, task queue.Task) error {
				handled = append(handled, task.DedupKey)
				return nil
			},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	handledN, parked := drainDueTasks(ctx, m, []string{"a"})

	if handledN != 1 || parked != 2 {
		t.Fatalf("handled=%d parked=%d, want 1/2", handledN, parked)
	}
	if len(handled) != 1 || handled[0] != "due" {
		t.Fatalf("handled keys = %v, want [due]", handled)
	}
	if q.Len("a") != 2 {
		t.Fatalf("queued after drain = %d, want 2 (future + nodeadline parked)", q.Len("a"))
	}
}

// A failing handler drops the task (logged) and does not stop the drain.
func TestDrainDueTasksHandlerFailureDropsTask(t *testing.T) {
	q := queue.New()
	ctx := context.Background()
	if err := q.Enqueue(ctx, queue.Task{AccountID: "a", Kind: queue.KindLift, DedupKey: "boom", Deadline: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.Enqueue(ctx, queue.Task{AccountID: "a", Kind: queue.KindLift, DedupKey: "fine", Deadline: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	m := maintenanceDeps{
		Queue: q,
		Handlers: map[queue.TaskKind]maintenanceHandler{
			queue.KindLift: func(_ context.Context, _ string, task queue.Task) error {
				if task.DedupKey == "boom" {
					return context.DeadlineExceeded
				}
				return nil
			},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	handled, _ := drainDueTasks(ctx, m, []string{"a"})
	if handled != 2 {
		t.Fatalf("handled = %d, want 2 (the failure is logged, not fatal)", handled)
	}
	if q.Len("a") != 0 {
		t.Fatalf("queued = %d, want 0 (a failed due task is dropped)", q.Len("a"))
	}
}
