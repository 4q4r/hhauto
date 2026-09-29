// Package webhook delivers optional event notifications: raw JSON POST
// (HMAC-signed, retried with backoff, per-endpoint proxy) plus telegram
// and discord presets.
package webhook

import (
	"context"
	"time"
)

// Event types (filterable per endpoint; empty filter = all).
const (
	EvApplySent        = "apply_sent"
	EvApplySkipped     = "apply_skipped"
	EvDailySummary     = "daily_summary"
	EvInterviewFound   = "interview_found"
	EvOfferReceived    = "offer_received"
	EvNeedsHuman       = "needs_human"
	EvCaptchaExhausted = "captcha_exhausted"
	EvAccountError     = "account_error"
	EvTestSolved       = "test_solved"
	EvContactCollected = "contact_collected"
	EvSetkaDMSent      = "setka_dm_sent" // T61: a Setka DM went out
)

// Event is the unit of delivery.
type Event struct {
	Type      string         `json:"type"`
	AccountID string         `json:"account_id,omitempty"`
	Time      time.Time      `json:"time"`
	Message   string         `json:"message"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Notifier fans an Event out to all configured, subscribed endpoints.
// Delivery is best-effort and non-blocking for the caller: failures go to
// the webhook outbox and are retried with backoff.
type Notifier interface {
	Emit(ev Event)
	Close(ctx context.Context) error
}
