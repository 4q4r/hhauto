// Package queue holds the typed in-process task queue with per-account FIFO
// + priority and dedup. Tasks are transient: interrupted work is recovered
// by the scheduler and intent reconciliation, not by persisting the queue.
package queue

import (
	"context"
	"time"
)

type TaskKind string

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
)

// Priority: lower value runs first (chat deadlines outrank applies).
type Priority int

const (
	PrioHealth Priority = 0
	PrioReview Priority = 1
	PrioChats  Priority = 2
	PrioApply  Priority = 3
	PrioEmail  Priority = 4
	PrioWarmup Priority = 5
	PrioLift   Priority = 6
)

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

type Queue interface {
	Enqueue(ctx context.Context, t Task) error // dedup: duplicate DedupKey is a no-op
	Dequeue(ctx context.Context, accountID string) (Task, bool)
	Len(accountID string) int
	// Close stops intake and drains up to drainTimeout.
	Close(ctx context.Context, drainTimeout time.Duration) error
}
