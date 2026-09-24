package queue

// Benchmarks for the per-account priority-queue hot path. Run with
// -benchtime=10000x for the fixed n=10000 shape from the test plan:
//
//	go test -bench 'BenchmarkEnqueue(Dequeue|Dedup)' -benchtime=10000x ./internal/queue/
//
// BenchmarkEnqueueDequeue: plain enqueue→dequeue cycle, DedupKey empty (no
// dedup bookkeeping). BenchmarkEnqueueDedup: the same cycle with a unique
// DedupKey per op — measures the dedup map insert plus the dequeue-side
// delete, keeping the queue O(1) in benchmark length.

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkEnqueueDequeue(b *testing.B) {
	q := New()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if err := q.Enqueue(ctx, Task{AccountID: "acc", Kind: KindApply, Priority: PrioApply}); err != nil {
			b.Fatal(err)
		}
		if _, ok := q.Dequeue(ctx, "acc"); !ok {
			b.Fatal("dequeue: no task")
		}
	}
}

func BenchmarkEnqueueDedup(b *testing.B) {
	q := New()
	ctx := context.Background()
	n := 0
	b.ReportAllocs()
	for b.Loop() {
		n++
		if err := q.Enqueue(ctx, Task{AccountID: "acc", Kind: KindApply, Priority: PrioApply,
			DedupKey: fmt.Sprintf("run|%d", n)}); err != nil {
			b.Fatal(err)
		}
		if _, ok := q.Dequeue(ctx, "acc"); !ok {
			b.Fatal("dequeue: no task")
		}
	}
}
