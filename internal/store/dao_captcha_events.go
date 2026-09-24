package store

import (
	"context"
	"fmt"
)

// CaptchaEventsAdd records a captcha-provider event (audit log) and fills
// e.ID.
func (db *DB) CaptchaEventsAdd(ctx context.Context, e *CaptchaEvent) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO captcha_events (account_id, provider_id, kind, ok, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id`,
		e.AccountID, e.ProviderID, e.Kind, boolInt(e.Ok), e.LatencyMS, nowOr(e.CreatedAt),
	).Scan(&e.ID)
	if err != nil {
		return fmt.Errorf("store: CaptchaEventsAdd: %w", err)
	}
	return nil
}
