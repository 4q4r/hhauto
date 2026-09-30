// Package queue implements the typed in-process task queue: per-account
// FIFO with priority, queued-scope dedup, ULIDs and drain-on-close.
//
// Wake-up design: each account has a `changed` broadcast channel that is
// closed and replaced under the account lock on every enqueue and on close.
// Waiters snapshot the channel under the lock and select on it together with
// ctx.Done(), which sync.Cond cannot do — that is why channels were chosen.
// The same close-and-replace pattern backs q.pendingChanged: every dequeue
// rebroadcasts it, so Close's drain loop can wait for "pending == 0" as a
// *current* condition. A one-shot "empty" signal would be wrong here:
// pending is non-monotonic while producers and consumers run concurrently,
// so an intermediate zero must not latch the signal forever.
//
// Close semantics (see Close): intake stops immediately, consumers keep
// pulling whatever is queued for up to drainTimeout (or until ctx is done),
// then unclaimed tasks are dropped — the scheduler and intent reconciliation
// recover them, the queue is transient by contract.
package queue

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"
)

// ErrClosed is returned by Enqueue after Close has been called.
var ErrClosed = errors.New("queue: closed")

type stage int32

const (
	stageOpen stage = iota
	stageDraining
	stageFinal
)

type item struct {
	task Task
	seq  uint64 // FIFO tie-breaker within one priority
	dkey string // registered dedup key ("" = none)
}

// itemHeap is a min-heap: lower Priority first, then earlier enqueue.
type itemHeap []*item

func (h itemHeap) Len() int { return len(h) }
func (h itemHeap) Less(i, j int) bool {
	if h[i].task.Priority != h[j].task.Priority {
		return h[i].task.Priority < h[j].task.Priority
	}
	return h[i].seq < h[j].seq
}
func (h itemHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *itemHeap) Push(x any)   { *h = append(*h, x.(*item)) }
func (h *itemHeap) Pop() (x any) {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return it
}

type accountQueue struct {
	mu      sync.Mutex
	items   itemHeap
	dedup   map[string]struct{}
	changed chan struct{} // closed-and-replaced to broadcast wakeups
}

type impl struct {
	mu       sync.Mutex // guards accounts map and stage
	accounts map[string]*accountQueue
	stage    stage

	seq            atomic.Uint64 // monotonic enqueue sequence
	pending        atomic.Int64  // queued-and-not-yet-dequeued items
	pendingChanged chan struct{} // closed-and-replaced on every dequeue
	newAccount     chan struct{} // closed-and-replaced when an account appears
}

// New returns a ready-to-use Queue.
func New() Queue {
	return &impl{
		accounts:       map[string]*accountQueue{},
		pendingChanged: make(chan struct{}),
		newAccount:     make(chan struct{}),
	}
}

func dedupKey(t Task) string {
	return t.AccountID + "\x00" + string(t.Kind) + "\x00" + t.DedupKey
}

// lookup returns the account queue without creating it.
func (q *impl) lookup(accountID string) *accountQueue {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stage == stageFinal {
		return nil
	}
	return q.accounts[accountID]
}

// Enqueue adds a task. Duplicate (AccountID,Kind,DedupKey) is a silent no-op
// while the earlier task is still queued. The ctx is accepted per the
// contract but never blocks: Enqueue has no waiting path. Task.ID is filled
// with a fresh ULID when empty; caller-provided IDs are preserved.
//
// The whole operation runs under q.mu (lock order q.mu → a.mu): once Close
// has left stageOpen under q.mu, pending can no longer increase, so Close's
// pending sample is race-free.
func (q *impl) Enqueue(_ context.Context, t Task) error {
	q.mu.Lock()
	if q.stage != stageOpen {
		q.mu.Unlock()
		return ErrClosed
	}
	a, ok := q.accounts[t.AccountID]
	if !ok {
		a = &accountQueue{dedup: map[string]struct{}{}, changed: make(chan struct{})}
		q.accounts[t.AccountID] = a
		close(q.newAccount)
		q.newAccount = make(chan struct{})
	}
	a.mu.Lock()
	defer q.mu.Unlock() // held through the a.mu section: stage + pending are one atomic step
	dk := ""
	if t.DedupKey != "" {
		dk = dedupKey(t)
		if _, dup := a.dedup[dk]; dup {
			a.mu.Unlock()
			return nil
		}
		a.dedup[dk] = struct{}{}
	}
	if t.ID == "" {
		t.ID = ulid.Make().String()
	}
	heap.Push(&a.items, &item{task: t, seq: q.seq.Add(1), dkey: dk})
	q.pending.Add(1) // under a.mu: Close's drain decision can't miss it
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
	return nil
}

// Dequeue blocks until a task for accountID is available, ctx is done, or
// the queue reaches its final stage. During the drain window tasks are still
// handed out; afterwards it returns (zero, false).
// dequeue hands out a popped item and rebroadcasts pendingChanged so a
// concurrent Close drain loop re-samples q.pending. Lock order is q.mu after
// a.mu (Enqueue takes them in the same order; nobody takes a.mu while
// holding q.mu without first acquiring it in that order), so there is no
// cycle.
func (q *impl) dequeueIt(a *accountQueue, it *item) Task {
	if it.dkey != "" {
		delete(a.dedup, it.dkey)
	}
	q.pending.Add(-1) // under a.mu: same atomicity as the matching Enqueue increment
	a.mu.Unlock()
	q.mu.Lock()
	close(q.pendingChanged)
	q.pendingChanged = make(chan struct{})
	q.mu.Unlock()
	return it.task
}

// Dequeue blocks until a task for accountID is available, ctx is done, or
// the queue reaches its final stage. During the drain window tasks are still
// handed out; afterwards it returns (zero, false).
func (q *impl) Dequeue(ctx context.Context, accountID string) (Task, bool) {
	for {
		a := q.lookup(accountID)
		if a == nil {
			// Account not seen yet: wait for one to appear (producers
			// create accounts on demand) or for ctx/close.
			q.mu.Lock()
			ch := q.newAccount
			st := q.stage
			q.mu.Unlock()
			if st != stageOpen {
				return Task{}, false
			}
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return Task{}, false
			}
		}

		a.mu.Lock()
		if len(a.items) > 0 {
			it := heap.Pop(&a.items).(*item)
			return q.dequeueIt(a, it), true
		}
		ch := a.changed
		a.mu.Unlock()

		q.mu.Lock()
		st := q.stage
		q.mu.Unlock()
		if st != stageOpen {
			return Task{}, false // nothing left to drain for this account
		}
		select {
		case <-ch:
			// New work may be available (or the queue closed): re-check.
		case <-ctx.Done():
			return Task{}, false
		}
	}
}

// Len reports the number of queued tasks for one account.
func (q *impl) Len(accountID string) int {
	a := q.lookup(accountID)
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.items)
}

// NextDeadline reports the earliest non-zero Deadline among the account's
// queued items (T71). A zero Deadline means "no deadline" and is ignored:
// such tasks never wake the daemon's maintenance pass.
func (q *impl) NextDeadline(accountID string) (time.Time, bool) {
	a := q.lookup(accountID)
	if a == nil {
		return time.Time{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var wake time.Time
	for _, it := range a.items {
		d := it.task.Deadline
		if d.IsZero() {
			continue
		}
		if wake.IsZero() || d.Before(wake) {
			wake = d
		}
	}
	return wake, !wake.IsZero()
}

// Close stops intake, waits up to drainTimeout (bounded by ctx) for
// consumers to pull the remaining tasks, then drops whatever is unclaimed —
// recovery is the scheduler's job, not the queue's. Close is idempotent and
// always returns nil: a best-effort drain is not a failure.
func (q *impl) Close(ctx context.Context, drainTimeout time.Duration) error {
	q.mu.Lock()
	if q.stage != stageOpen {
		q.mu.Unlock()
		return nil
	}
	q.stage = stageDraining
	// From here no pending can appear: Enqueue checks the stage under q.mu
	// in the same critical section as its pending increment. Wake waiters
	// for not-yet-created accounts — no account will appear anymore.
	close(q.newAccount)
	q.newAccount = make(chan struct{})
	q.mu.Unlock()

	// Drain: wait for the *current* pending to reach zero. pending only
	// decreases from here, and every dequeue rebroadcasts pendingChanged —
	// unlike a one-shot "empty" latch this cannot be satisfied by an
	// intermediate zero from before Close started.
	if drainTimeout > 0 {
		timer := time.NewTimer(drainTimeout)
		defer timer.Stop()
	drain:
		for {
			q.mu.Lock()
			p := q.pending.Load()
			ch := q.pendingChanged
			q.mu.Unlock()
			if p == 0 {
				break
			}
			select {
			case <-ch: // pending changed: re-sample
			case <-timer.C:
				break drain
			case <-ctx.Done():
				break drain
			}
		}
	}

	q.mu.Lock()
	q.stage = stageFinal
	close(q.newAccount) // release waiters that sampled stageOpen earlier
	q.newAccount = make(chan struct{})
	accounts := q.accounts
	q.mu.Unlock()

	for _, a := range accounts {
		a.mu.Lock()
		a.items = nil
		a.dedup = map[string]struct{}{}
		close(a.changed) // wake all Dequeue waiters
		a.changed = make(chan struct{})
		a.mu.Unlock()
	}
	// q.pending is not rewound for dropped tasks: after stageFinal the queue
	// is dead and nothing reads the counter again.
	return nil
}
