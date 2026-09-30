// Package queue holds the typed in-process task queue with per-account FIFO
// + priority and dedup. Tasks are transient: interrupted work is recovered
// by the scheduler and intent reconciliation, not by persisting the queue.
package queue

import (
	"context"
	"time"
)

// TaskKind is the stable identifier of one task family; features define the
// payload shapes behind their kinds.
type TaskKind string

// Task kinds registered by the CLI wiring (T13) and their features.
const (
	KindLift            TaskKind = "lift"
	KindSearch          TaskKind = "search"
	KindApply           TaskKind = "apply"
	KindChatSync        TaskKind = "chat_sync"
	KindWarmupPing      TaskKind = "warmup_ping"
	KindSolveTest       TaskKind = "solve_test"
	KindCollectContacts TaskKind = "collect_contacts"
	KindSendEmail       TaskKind = "send_email"
	KindClassify        TaskKind = "classify"
	KindSetkaDM         TaskKind = "setka_dm" // T61: post-apply Setka DM flow
)

// Priority orders tasks within an account: lower value runs first (chat
// deadlines outrank applies).
type Priority int

// Priority levels, one per task family.
const (
	PrioHealth Priority = 0
	PrioReview Priority = 1
	PrioChats  Priority = 2
	PrioApply  Priority = 3
	PrioEmail  Priority = 4
	PrioWarmup Priority = 5
	PrioLift   Priority = 6
	PrioSetka  Priority = 7 // least urgent: human-paced DMs (T61)
)

// Task is one queued unit of work for a single account.
type Task struct {
	ID        string // ULID
	AccountID string
	Kind      TaskKind
	RunID     string // "" for non-run tasks
	DedupKey  string // queued dedup per (AccountID,Kind,DedupKey)
	Priority  Priority
	Deadline  time.Time
	Payload   any // typed payload per Kind, defined by feature packages
}

// Queue is the in-process per-account task queue the engine consumes.
type Queue interface {
	Enqueue(ctx context.Context, t Task) error // dedup: duplicate DedupKey is a no-op
	Dequeue(ctx context.Context, accountID string) (Task, bool)
	Len(accountID string) int
	// NextDeadline reports the earliest non-zero Deadline queued for the
	// account (T71: the daemon's maintenance wake-up). Zero-deadline tasks
	// are ignored; ok=false when none is queued.
	NextDeadline(accountID string) (time.Time, bool)
	// Close stops intake and drains up to drainTimeout.
	Close(ctx context.Context, drainTimeout time.Duration) error
}
