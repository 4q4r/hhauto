package cli

// storeOutbox adapts the store's webhook_outbox DAOs onto the
// webhook.Outbox seam the engine's outbox worker drives.
//
// Documented deviation: the store contract exposes Add / Due /
// MarkDelivered only — there is no MarkFailed DAO (no way to persist a
// next-retry stamp). The bridge therefore keeps the retry backoff
// in-process: MarkFailed delays the item in memory for the current run;
// after a restart, due items replay immediately (attempt counters reset).
// Delivery stays at-least-once either way; endpoints are best-effort by
// contract.
//
// Type note: store.OutboxItem.Payload is TEXT in SQLite while
// webhook.OutboxItem.Payload is []byte — the bridge converts at the seam.

import (
	"context"
	"sync"
	"time"

	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

const outboxCallTimeout = 5 * time.Second

type storeOutbox struct {
	db *store.DB

	mu      sync.Mutex
	delayed map[int64]time.Time // item id → next retry (in-process overlay)
}

func newStoreOutbox(db *store.DB) *storeOutbox {
	return &storeOutbox{db: db, delayed: map[int64]time.Time{}}
}

func (o *storeOutbox) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), outboxCallTimeout)
}

// Add persists a new parked payload.
func (o *storeOutbox) Add(endpointID string, payload []byte) error {
	ctx, cancel := o.ctx()
	defer cancel()
	return o.db.WebhookOutboxAdd(ctx, &store.OutboxItem{EndpointID: endpointID, Payload: string(payload)})
}

// Due lists undelivered items whose in-memory retry stamp (if any) has
// passed. The DB query over-fetches by the delayed count so post-filtering
// cannot starve the limit.
func (o *storeOutbox) Due(now time.Time, limit int) ([]webhook.OutboxItem, error) {
	o.mu.Lock()
	extra := len(o.delayed)
	o.mu.Unlock()
	if extra > outboxDueOverfetchCap {
		extra = outboxDueOverfetchCap
	}

	ctx, cancel := o.ctx()
	defer cancel()
	rows, err := o.db.WebhookOutboxDue(ctx, now, limit+extra)
	if err != nil {
		return nil, err
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]webhook.OutboxItem, 0, len(rows))
	for _, r := range rows {
		if d, ok := o.delayed[r.ID]; ok && d.After(now) {
			continue
		}
		out = append(out, webhook.OutboxItem{ID: r.ID, EndpointID: r.EndpointID, Payload: []byte(r.Payload)})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// MarkDelivered clears the item for good.
func (o *storeOutbox) MarkDelivered(id int64) error {
	o.mu.Lock()
	delete(o.delayed, id)
	o.mu.Unlock()

	ctx, cancel := o.ctx()
	defer cancel()
	return o.db.WebhookOutboxMarkDelivered(ctx, id)
}

// MarkFailed schedules the next in-process retry (see package comment).
func (o *storeOutbox) MarkFailed(id int64, nextRetry time.Time) error {
	o.mu.Lock()
	o.delayed[id] = nextRetry
	o.mu.Unlock()
	return nil
}

// outboxDueOverfetchCap bounds the over-fetch headroom.
const outboxDueOverfetchCap = 256

var _ webhook.Outbox = (*storeOutbox)(nil)
