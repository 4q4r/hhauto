package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// WebhookOutboxAdd enqueues an outbox item; i.ID is filled, attempts start
// at 0 and delivered at false.
func (db *DB) WebhookOutboxAdd(ctx context.Context, i *OutboxItem) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO webhook_outbox (endpoint_id, payload, attempts, next_retry_at, delivered, created_at)
		VALUES (?, ?, 0, ?, 0, ?)
		RETURNING id`,
		i.EndpointID, i.Payload, fmtTimeArg(i.NextRetryAt), nowOr(i.CreatedAt),
	).Scan(&i.ID)
	if err != nil {
		return fmt.Errorf("store: WebhookOutboxAdd: %w", err)
	}
	return nil
}

// WebhookOutboxDue returns undelivered items whose next_retry_at has passed
// (or is unset), oldest retry first.
func (db *DB) WebhookOutboxDue(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, endpoint_id, payload, attempts, next_retry_at, delivered, created_at
		FROM webhook_outbox
		WHERE delivered = 0 AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY next_retry_at, id
		LIMIT ?`, fmtTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: WebhookOutboxDue: %w", err)
	}
	defer rows.Close()

	var out []OutboxItem
	for rows.Next() {
		i, err := scanOutboxItem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: WebhookOutboxDue scan: %w", err)
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// WebhookOutboxMarkDelivered flags the item delivered.
func (db *DB) WebhookOutboxMarkDelivered(ctx context.Context, id int64) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE webhook_outbox SET delivered = 1 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: WebhookOutboxMarkDelivered: %w", err)
	}
	return nil
}

func scanOutboxItem(rs rowScanner) (*OutboxItem, error) {
	var i OutboxItem
	var delivered sql.NullInt64
	var retry, created sql.NullString
	if err := rs.Scan(&i.ID, &i.EndpointID, &i.Payload, &i.Attempts, &retry, &delivered, &created); err != nil {
		return nil, err
	}
	i.Delivered = delivered.Int64 != 0
	var err error
	if i.NextRetryAt, err = ptrTimeFromNull(retry); err != nil {
		return nil, err
	}
	if i.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	return &i, nil
}
