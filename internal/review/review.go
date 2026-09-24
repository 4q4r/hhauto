// Package review is the human-in-the-loop contour: a persistent queue of
// items that need a person (dead sessions, exhausted captcha chains,
// ambiguous outcomes), CLI resolve/ignore, SLA tracking with a single
// re-notification per item, and webhook fan-out of needs_human events.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"hhauto/internal/audit"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// featureName is the feature_state namespace used for the SLA latch.
const featureName = "review"

// dedupScanLimit caps the open-items idempotency scan per Add/ScanSLA
// account pass. Open review queues are small (items are human-resolved);
// past this cap a duplicate could slip through — accepted in exchange for
// a bounded scan (the DAO has no "open by target" lookup).
const dedupScanLimit = 500

// Service owns the review workflow. The notifier may be nil (storage-only
// operation, tests); the logger may be nil (slog.Default is used).
type Service struct {
	notify webhook.Notifier
	log    *slog.Logger
}

// New builds a review service.
func New(notify webhook.Notifier, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{notify: notify, log: log}
}

// Add records an item that needs human attention and emits EvNeedsHuman.
//
// Idempotency: one open item per (account, kind, target). The target is
// the "target" field of the JSON payload when present, otherwise the whole
// payload string. A duplicate of an already-open item returns its id
// without a second notification; a resolved/ignored predecessor does not
// absorb a new occurrence.
func (s *Service) Add(ctx context.Context, db *store.DB, accountID, kind, reason, payload string) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("review: nil store")
	}
	target := targetOf(payload)

	// Idempotency scan over open items (accounts hold few open items; the
	// generous limit keeps the scan complete in practice).
	open, err := db.ReviewItemsOpen(ctx, accountID, dedupScanLimit)
	if err != nil {
		return 0, fmt.Errorf("review: open items scan: %w", err)
	}
	for _, it := range open {
		if it.Kind == kind && targetOf(it.Payload) == target {
			s.log.Debug("review: duplicate open item, not re-added", "account", accountID, "kind", kind, "id", it.ID)
			return it.ID, nil
		}
	}

	id, err := db.ReviewItemsAdd(ctx, accountID, kind, reason, payload)
	if err != nil {
		return 0, fmt.Errorf("review: add: %w", err)
	}
	s.notifyNeedsHuman(accountID, id, kind, reason, target)
	return id, nil
}

func (s *Service) notifyNeedsHuman(accountID string, id int64, kind, reason, target string) {
	if s.notify == nil {
		return
	}
	s.notify.Emit(webhook.Event{
		Type:      webhook.EvNeedsHuman,
		AccountID: accountID,
		Time:      time.Now(),
		Message:   fmt.Sprintf("needs human [%s] %s (review #%d)", kind, reason, id),
		Payload: map[string]any{
			"review_id": id,
			"kind":      kind,
			"target":    target,
		},
	})
}

// targetOf extracts the dedup target from a payload: the JSON "target"
// field when the payload is an object carrying one, else the payload as-is.
func targetOf(payload string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &m); err == nil {
		if raw, ok := m["target"]; ok {
			var s string
			if jerr := json.Unmarshal(raw, &s); jerr == nil {
				return s
			}
		}
	}
	return payload
}

// Resolve closes an item as resolved with a note.
func (s *Service) Resolve(ctx context.Context, db *store.DB, id int64, resolution string) error {
	if db == nil {
		return fmt.Errorf("review: nil store")
	}
	if err := db.ReviewItemsResolve(ctx, id, resolution); err != nil {
		return fmt.Errorf("review: resolve %d: %w", id, err)
	}
	return nil
}

// Ignore closes an item as deliberately not acted upon. The store contract
// has a single close transition, so the intent is recorded in the
// resolution note ("ignored").
func (s *Service) Ignore(ctx context.Context, db *store.DB, id int64) error {
	return s.Resolve(ctx, db, id, "ignored")
}

// ScanSLA walks every account's open items and breaches the SLA for items
// older than slaHours: the breach counter is incremented (audit contract)
// and one re-notification is emitted per item — the feature_state latch
// ("sla_notified:<id>") prevents repeat notifications on later scans.
func (s *Service) ScanSLA(ctx context.Context, db *store.DB, slaHours int, accounts []string, now time.Time) error {
	if db == nil {
		return fmt.Errorf("review: nil store")
	}
	if slaHours <= 0 {
		slaHours = 24
	}
	deadline := now.Add(-time.Duration(slaHours) * time.Hour)
	day := audit.DayAt(now)

	for _, accountID := range accounts {
		open, err := db.ReviewItemsOpen(ctx, accountID, dedupScanLimit)
		if err != nil {
			return fmt.Errorf("review: sla scan %s: %w", accountID, err)
		}
		for _, it := range open {
			if it.CreatedAt.After(deadline) {
				continue
			}
			key := "sla_notified:" + strconv.FormatInt(it.ID, 10)
			if _, exists, err := db.FeatureStateGet(ctx, featureName, accountID, key); err != nil {
				return fmt.Errorf("review: sla latch read: %w", err)
			} else if exists {
				continue
			}
			if err := audit.IncrDay(ctx, db, audit.KeyReviewSLABreach, day); err != nil {
				return fmt.Errorf("review: sla metric: %w", err)
			}
			if err := db.FeatureStateSet(ctx, featureName, accountID, key, day); err != nil {
				return fmt.Errorf("review: sla latch write: %w", err)
			}
			s.notifyNeedsHuman(accountID, it.ID, it.Kind, "SLA breached: "+it.Reason, targetOf(it.Payload))
			s.log.Warn("review: SLA breached", "account", accountID, "id", it.ID, "kind", it.Kind)
		}
	}
	return nil
}
