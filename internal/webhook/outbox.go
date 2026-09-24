package webhook

import "time"

// OutboxItem is one parked delivery waiting for a later retry by the outbox
// worker (owned by internal/store in a later wave).
type OutboxItem struct {
	ID         int64
	EndpointID string
	Payload    []byte // exact HTTP body to replay for this endpoint
}

// Outbox persists deliveries that exhausted their in-process retries so a
// later worker can redeliver them. A nil Outbox means in-memory retries only.
type Outbox interface {
	Add(endpointID string, payload []byte) error
	Due(now time.Time, limit int) ([]OutboxItem, error)
	MarkDelivered(id int64) error
	MarkFailed(id int64, nextRetry time.Time) error
}
