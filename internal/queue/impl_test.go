package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func newTask(accountID string, prio Priority, dedupKey string) Task {
	return Task{
		AccountID: accountID,
		Kind:      KindApply,
		DedupKey:  dedupKey,
		Priority:  prio,
		Payload:   map[string]int{"n": int(prio)},
	}
}

func TestPriorityThenFIFO(t *testing.T) {
	q := New()
	defer q.Close(context.Background(), 0)

	// Enqueue out of priority order; equal priorities must stay FIFO.
	for _, prio := range []Priority{PrioLift, PrioChats, PrioHealth, PrioChats, PrioApply} {
		if err := q.Enqueue(context.Background(), newTask("a", prio, "")); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if got := q.Len("a"); got != 5 {
		t.Fatalf("Len(a) = %d, want 5", got)
	}

	want := []Priority{PrioHealth, PrioChats, PrioChats, PrioApply, PrioLift}
	var got []Priority
	for range want {
		task, ok := q.Dequeue(context.Background(), "a")
		if !ok {
			t.Fatalf("Dequeue returned !ok after %d of %d tasks", len(got), len(want))
		}
		got = append(got, task.Priority)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestAccountsAreIsolated(t *testing.T) {
	q := New()
	defer q.Close(context.Background(), 0)

	if err := q.Enqueue(context.Background(), newTask("a", PrioApply, "")); err != nil {
		t.Fatalf("Enqueue a: %v", err)
	}
	if err := q.Enqueue(context.Background(), newTask("b", PrioHealth, "")); err != nil {
		t.Fatalf("Enqueue b: %v", err)
	}
	if got := q.Len("b"); got != 1 {
		t.Fatalf("Len(b) = %d, want 1", got)
	}
	if got := q.Len("unknown"); got != 0 {
		t.Fatalf("Len(unknown) = %d, want 0", got)
	}
	task, ok := q.Dequeue(context.Background(), "b")
	if !ok || task.AccountID != "b" || task.Priority != PrioHealth {
		t.Fatalf("Dequeue(b) = (%+v, %v), want account b health task", task, ok)
	}
}

func TestDedupIsQueuedScopedNoOp(t *testing.T) {
	q := New()
	ctx := context.Background()

	t1 := newTask("a", PrioApply, "vac-1")
	if err := q.Enqueue(ctx, t1); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	// Duplicate (AccountID,Kind,DedupKey) is a no-op, not an error.
	if err := q.Enqueue(ctx, newTask("a", PrioApply, "vac-1")); err != nil {
		t.Fatalf("duplicate Enqueue: %v", err)
	}
	if got := q.Len("a"); got != 1 {
		t.Fatalf("Len(a) = %d, want 1 (duplicate must be dropped)", got)
	}

	// Same key, different account or kind: distinct task.
	if err := q.Enqueue(ctx, newTask("b", PrioApply, "vac-1")); err != nil {
		t.Fatalf("Enqueue other account: %v", err)
	}
	chat := t1
	chat.Kind = KindChatSync
	if err := q.Enqueue(ctx, chat); err != nil {
		t.Fatalf("Enqueue other kind: %v", err)
	}
	if got := q.Len("a"); got != 2 {
		t.Fatalf("Len(a) = %d, want 2", got)
	}

	// Dedup key applies only while queued: after Dequeue the key is free.
	if _, ok := q.Dequeue(ctx, "a"); !ok {
		t.Fatal("Dequeue(a) = !ok, want task")
	}
	if got := q.Len("a"); got != 1 {
		t.Fatalf("Len(a) after dequeue = %d, want 1", got)
	}
	if err := q.Enqueue(ctx, newTask("a", PrioApply, "vac-1")); err != nil {
		t.Fatalf("re-Enqueue after dequeue: %v", err)
	}
	if got := q.Len("a"); got != 2 {
		t.Fatalf("Len(a) after re-enqueue = %d, want 2 (dedup is queued-scoped)", got)
	}

	// Empty DedupKey: every task is unique.
	for range 3 {
		if err := q.Enqueue(ctx, newTask("c", PrioWarmup, "")); err != nil {
			t.Fatalf("Enqueue empty dedup: %v", err)
		}
	}
	if got := q.Len("c"); got != 3 {
		t.Fatalf("Len(c) = %d, want 3 (empty key never dedups)", got)
	}
}

func TestTaskIDIsULIDWhenEmpty(t *testing.T) {
	q := New()
	defer q.Close(context.Background(), 0)
	ctx := context.Background()

	if err := q.Enqueue(ctx, newTask("a", PrioApply, "")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	task, ok := q.Dequeue(ctx, "a")
	if !ok {
		t.Fatal("Dequeue = !ok, want task")
	}
	if _, err := ulid.Parse(task.ID); err != nil {
		t.Fatalf("Task.ID %q is not a ULID: %v", task.ID, err)
	}

	// A caller-provided ID is preserved.
	pre := newTask("a", PrioApply, "")
	pre.ID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := q.Enqueue(ctx, pre); err != nil {
		t.Fatalf("Enqueue with preset ID: %v", err)
	}
	got, ok := q.Dequeue(ctx, "a")
	if !ok || got.ID != pre.ID {
		t.Fatalf("preset ID = %q (ok=%v), want %q", got.ID, ok, pre.ID)
	}
}

func TestDequeueBlocksAndRespectsContext(t *testing.T) {
	q := New()
	defer q.Close(context.Background(), 0)
	ctx := context.Background()

	// Blocked Dequeue wakes up when a task arrives.
	done := make(chan Task, 1)
	go func() {
		task, ok := q.Dequeue(ctx, "a")
		if ok {
			done <- task
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the consumer block first
	if err := q.Enqueue(ctx, newTask("a", PrioApply, "")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case task := <-done:
		if task.AccountID != "a" {
			t.Fatalf("woken task account = %q, want a", task.AccountID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Dequeue did not wake on Enqueue")
	}

	// Cancelled context unblocks Dequeue with !ok.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := q.Dequeue(cctx, "a"); ok {
		t.Fatal("Dequeue with cancelled ctx returned ok, want false")
	}
}

func TestCloseDrainSemantics(t *testing.T) {
	ctx := context.Background()

	// Case 1: consumer keeps pulling during the drain window.
	q := New()
	for i := range 10 {
		if err := q.Enqueue(ctx, newTask("a", PrioApply, "")); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		_ = i
	}
	var got syncGuard
	consumerDone := make(chan int, 1)
	go func() {
		for {
			task, ok := q.Dequeue(ctx, "a")
			if !ok {
				consumerDone <- got.n
				return
			}
			got.add(1)
			time.Sleep(10 * time.Millisecond) // simulate work
			_ = task
		}
	}()
	if err := q.Close(ctx, 500*time.Millisecond); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case n := <-consumerDone:
		if n != 10 {
			t.Fatalf("consumer processed %d of 10 tasks before close returned", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not finish")
	}
	if _, ok := q.Dequeue(ctx, "a"); ok {
		t.Error("Dequeue after Close returned ok, want false")
	}
	if err := q.Enqueue(ctx, newTask("a", PrioApply, "")); !errors.Is(err, ErrClosed) {
		t.Errorf("Enqueue after Close = %v, want ErrClosed", err)
	}

	// Case 2: nothing consumes — after the drain window the leftovers are
	// dropped (the scheduler recovers them).
	q2 := New()
	for range 5 {
		if err := q2.Enqueue(ctx, newTask("a", PrioApply, "")); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if err := q2.Close(ctx, 50*time.Millisecond); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := q2.Len("a"); got != 0 {
		t.Errorf("Len after Close = %d, want 0 (unclaimed tasks are dropped)", got)
	}
	if _, ok := q2.Dequeue(ctx, "a"); ok {
		t.Error("Dequeue after drop-Close returned ok, want false")
	}

	// Case 3: Close is idempotent.
	if err := q2.Close(ctx, 0); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}

	// Case 4: zero drainTimeout drops immediately.
	q3 := New()
	if err := q3.Enqueue(ctx, newTask("a", PrioApply, "")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q3.Close(ctx, 0); err != nil {
		t.Fatalf("Close(0): %v", err)
	}
	if got := q3.Len("a"); got != 0 {
		t.Errorf("Len after Close(0) = %d, want 0", got)
	}
}

type syncGuard struct {
	mu sync.Mutex
	n  int
}

func (g *syncGuard) add(d int) {
	g.mu.Lock()
	g.n += d
	g.mu.Unlock()
}

func (g *syncGuard) val() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

func TestConcurrentProducersConsumers(t *testing.T) {
	q := New()
	ctx := context.Background()

	const (
		producers = 4
		consumers = 3
		accounts  = 2
		perProd   = 25
	)
	total := producers * perProd

	var prodWG sync.WaitGroup
	for p := range producers {
		prodWG.Add(1)
		go func(p int) {
			defer prodWG.Done()
			for i := range perProd {
				acct := string(rune('a' + p%accounts))
				task := newTask(acct, Priority(i%int(PrioLift+1)), "")
				if err := q.Enqueue(ctx, task); err != nil {
					t.Errorf("producer Enqueue: %v", err)
					return
				}
			}
		}(p)
	}

	var mu sync.Mutex
	seenIDs := make(map[string]bool, total) // every ID exactly once
	consumed := 0
	accountTally := map[string]int{}

	consumerCtx, cancel := context.WithCancel(ctx)
	var consWG sync.WaitGroup
	consAccount := func(accountID string) {
		defer consWG.Done()
		for {
			task, ok := q.Dequeue(consumerCtx, accountID)
			if !ok {
				return
			}
			mu.Lock()
			if seenIDs[task.ID] {
				t.Errorf("task %s consumed twice", task.ID)
			}
			seenIDs[task.ID] = true
			consumed++
			accountTally[task.AccountID]++
			mu.Unlock()
		}
	}
	for range consumers {
		consWG.Add(1)
		go consAccount("a")
	}
	// A second consumer family for the other account.
	for range consumers {
		consWG.Add(1)
		go consAccount("b")
	}

	prodWG.Wait()
	closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer closeCancel()
	if err := q.Close(closeCtx, time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancel()
	consWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if consumed != total {
		t.Fatalf("consumed %d tasks, want exactly %d (no task lost or duplicated)", consumed, total)
	}
	if len(seenIDs) != total {
		t.Fatalf("%d distinct task IDs, want %d", len(seenIDs), total)
	}
	for _, acct := range []string{"a", "b"} {
		if accountTally[acct] != total/accounts {
			t.Fatalf("account %q consumed %d tasks, want %d", acct, accountTally[acct], total/accounts)
		}
	}
}

func TestDedupKeyReusableAfterPop(t *testing.T) {
	// Contract: dedup is queued-scoped — after a task is popped its
	// (AccountID,Kind,DedupKey) is free again, a re-Enqueue must queue a
	// fresh task.
	q := New()
	defer q.Close(context.Background(), 0)
	ctx := context.Background()

	if err := q.Enqueue(ctx, newTask("a", PrioApply, "vac-9")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, ok := q.Dequeue(ctx, "a")
	if !ok {
		t.Fatal("Dequeue = !ok, want task")
	}
	if err := q.Enqueue(ctx, newTask("a", PrioApply, "vac-9")); err != nil {
		t.Fatalf("re-Enqueue after pop: %v", err)
	}
	if got := q.Len("a"); got != 1 {
		t.Fatalf("Len(a) = %d, want 1 (key was released by the pop)", got)
	}
	second, ok := q.Dequeue(ctx, "a")
	if !ok {
		t.Fatal("Dequeue = !ok, want re-enqueued task")
	}
	if second.ID == first.ID {
		t.Fatalf("re-enqueued task reused ID %q, want a fresh ULID", first.ID)
	}
}

func TestCloseOnEmptyQueueReturnsFast(t *testing.T) {
	// Regression: with nothing ever pending, Close must not burn the whole
	// drain timeout waiting on an `empty` signal that will never fire (the
	// channel is only closed on Dequeue/drop transitions).
	q := New()

	start := time.Now()
	const drainTimeout = 3 * time.Second
	if err := q.Close(context.Background(), drainTimeout); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Close on never-used queue took %v, want << drainTimeout %v", elapsed, drainTimeout)
	}
}

func TestCloseWakesUnknownAccountWaiter(t *testing.T) {
	// Regression: a Dequeue waiting for a never-enqueued account must be
	// released by Close even without ctx cancellation.
	q := New()
	returned := make(chan bool, 1)
	go func() {
		_, ok := q.Dequeue(context.Background(), "ghost")
		returned <- ok
	}()
	time.Sleep(50 * time.Millisecond) // let the consumer block first

	if err := q.Close(context.Background(), 0); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("Dequeue = ok, want false after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dequeue for unknown account leaked past Close without ctx cancellation")
	}
}

// TestConcurrentEnqueueDequeueCloseStorm exercises producers, consumers and
// Close(drain) running at the same time. Guarantees checked: consumed IDs
// are unique and a subset of accepted enqueues, nothing survives past final
// close, and every goroutine terminates on its own (no lost wakeups, no ctx
// cancellation needed).
func TestConcurrentEnqueueDequeueCloseStorm(t *testing.T) {
	q := New()
	ctx := context.Background()

	const (
		producers = 4
		consumers = 3
		perProd   = 20
	)

	var mu sync.Mutex
	seenIDs := map[string]bool{}
	accepted := 0 // successful Enqueues
	consumed := 0

	var prodWG sync.WaitGroup
	for p := range producers {
		prodWG.Add(1)
		go func(p int) {
			defer prodWG.Done()
			for i := range perProd {
				acct := string(rune('a' + p%2))
				err := q.Enqueue(ctx, newTask(acct, Priority(i%int(PrioLift+1)), ""))
				mu.Lock()
				if err == nil {
					accepted++
				}
				mu.Unlock()
				if err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("producer Enqueue = %v, want nil or ErrClosed", err)
					return
				}
				time.Sleep(time.Millisecond) // stretch the race window
			}
		}(p)
	}

	// Close races the producers: it starts while they are still enqueueing.
	go func() {
		time.Sleep(20 * time.Millisecond)
		if err := q.Close(ctx, 150*time.Millisecond); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	var consWG sync.WaitGroup
	consDone := make(chan struct{})
	for c := range consumers {
		consWG.Add(1)
		go func(c int) {
			defer consWG.Done()
			acct := string(rune('a' + c%2))
			for {
				task, ok := q.Dequeue(ctx, acct) // plain Background ctx: must return by itself
				if !ok {
					return
				}
				mu.Lock()
				if seenIDs[task.ID] {
					t.Errorf("task %s consumed twice", task.ID)
				}
				seenIDs[task.ID] = true
				consumed++
				mu.Unlock()
			}
		}(c)
	}
	go func() {
		consWG.Wait()
		close(consDone)
	}()

	// No lost wakeups: every goroutine finishes on its own, without ctx
	// cancellation and within a bounded timeout.
	prodDone := make(chan struct{})
	go func() {
		prodWG.Wait()
		close(prodDone)
	}()
	select {
	case <-prodDone:
	case <-time.After(5 * time.Second):
		t.Fatal("producers did not terminate within 5s")
	}
	select {
	case <-consDone:
	case <-time.After(5 * time.Second):
		t.Fatal("consumers did not terminate within 5s of Close (lost wakeup leak)")
	}

	mu.Lock()
	defer mu.Unlock()
	if consumed > accepted {
		t.Fatalf("consumed %d > accepted %d", consumed, accepted)
	}
	if len(seenIDs) != consumed {
		t.Fatalf("%d distinct consumed IDs != consumed count %d", len(seenIDs), consumed)
	}
	if l := q.Len("a") + q.Len("b"); l != 0 {
		t.Errorf("Len after final close = %d, want 0", l)
	}
	if err := q.Close(ctx, 0); err != nil {
		t.Errorf("idempotent second Close = %v, want nil", err)
	}
}

func TestConcurrentDedupEnqueuesProduceExactlyOne(t *testing.T) {
	q := New()
	defer q.Close(context.Background(), 0)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.Enqueue(ctx, newTask("a", PrioApply, "same-key")); err != nil {
				t.Errorf("Enqueue: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := q.Len("a"); got != 1 {
		t.Fatalf("Len(a) = %d, want 1 (racy duplicate Enqueue still yields one task)", got)
	}
}
