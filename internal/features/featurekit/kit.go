// Package featurekit holds the small helper pieces shared by the feature
// packages (autolift, multiapply, relevance, emplrating): the injected
// pacing/captcha/client seams, the intent idempotency flow over the store
// DAO, the outgoing-text link ban and a few stable key builders.
//
// It is an implementation helper, NOT a package contract: feature packages
// may reshape it freely as long as internal/* contracts stay untouched.
package featurekit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"hhauto/internal/captcha"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// APIFor resolves the per-account hh client. err != nil means the account
// has no usable client right now (not configured, construction failed).
type APIFor func(accountID string) (hhapi.HHAPI, error)

// ChainFor resolves the captcha provider chain for an account.
// ok=false means no chain is configured: captcha failures are terminal.
type ChainFor func(accountID string) (captcha.Chain, bool)

// --- Intent flow ---

// IntentStep is the outcome of claiming an idempotency key.
type IntentStep int

const (
	// IntentProceed means a fresh pending intent was created; run the side effect.
	IntentProceed IntentStep = iota
	// IntentSkipPending means an identical intent is still in flight — skip.
	IntentSkipPending
	// IntentReconcileFirst means the intent already completed — the caller
	// must verify the real (server-side) state before doing anything; the
	// store refuses to re-arm a done key.
	IntentReconcileFirst
)

// IntentClaim is the result of BeginIntent.
type IntentClaim struct {
	Step IntentStep
	ID   string // intent id (ULID) when Step == IntentProceed
}

// BeginIntent claims the idempotency key and maps the three store outcomes
// onto the intent steps. Any other store error is returned as-is.
func BeginIntent(ctx context.Context, db *store.DB, key, payload string) (IntentClaim, error) {
	id, err := db.IntentsBegin(ctx, key, "pending", payload)
	switch {
	case err == nil:
		return IntentClaim{Step: IntentProceed, ID: id}, nil
	case errors.Is(err, store.ErrIntentPending):
		return IntentClaim{Step: IntentSkipPending}, nil
	case errors.Is(err, store.ErrIntentDone):
		return IntentClaim{Step: IntentReconcileFirst}, nil
	default:
		return IntentClaim{}, fmt.Errorf("featurekit: begin intent %s: %w", key, err)
	}
}

// FinishIntent settles a claimed intent: done when err == nil, failed
// otherwise. Passing a claim whose Step != IntentProceed is a no-op.
func FinishIntent(ctx context.Context, db *store.DB, c IntentClaim, err error) error {
	if c.Step != IntentProceed || c.ID == "" {
		return nil
	}
	if err != nil {
		return db.IntentsMarkFailed(ctx, c.ID)
	}
	return db.IntentsMarkDone(ctx, c.ID)
}

// AbandonIntent marks the intent abandoned so the key can be re-armed by a
// later Begin. Passing a claim whose Step != IntentProceed is a no-op.
func AbandonIntent(ctx context.Context, db *store.DB, c IntentClaim) error {
	if c.Step != IntentProceed || c.ID == "" {
		return nil
	}
	return db.IntentsMarkAbandoned(ctx, c.ID)
}

// IntentPayload marshals v as the stored intent payload; a marshal failure
// degrades to a compact error string so claiming never blocks on it.
func IntentPayload(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"payload_error":%q}`, err.Error())
	}
	return string(b)
}

// --- Outgoing text rule ---

// ErrLinkFound is returned by NoLinksRule when banned link/contact patterns
// are present in outgoing text (hh shadow-bans such messages).
var ErrLinkFound = errors.New("featurekit: outgoing text must not contain links or contact handles")

// Pacer spaces outgoing actions per account (humanized delays, active
// windows). A nil Pacer means "no delay" — callers must tolerate that.
type Pacer interface {
	Wait(ctx context.Context, accountID string) error
}

// NoLinksRule rejects outgoing texts (cover letters, chat messages) that
// contain http/https URLs, www hosts, t.me / telegram.me links or @handle
// tokens (which also covers email addresses).
func NoLinksRule(text string) error {
	lower := strings.ToLower(text)
	for _, pat := range []string{"http://", "https://", "www.", "t.me/", "telegram.me/"} {
		if i := strings.Index(lower, pat); i >= 0 {
			return fmt.Errorf("%w: %s", ErrLinkFound, fragment(lower, i))
		}
	}
	if i := strings.IndexAny(text, "@"); i >= 0 {
		return fmt.Errorf("%w: %s", ErrLinkFound, fragment(text, i))
	}
	return nil
}

// fragment returns a short slice of s around position i for error text.
func fragment(s string, i int) string {
	start := i
	if start > 20 {
		start = i - 20
	}
	end := i + 24
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// ResumeLiftable reports whether a resume status means "published": hh
// wraps status objects so either the dictionary id ("published") or the
// Russian display name ("Опубликовано") may surface first. Shared by
// autolift and multiapply (single source of truth, Wave 3 review fix 6).
func ResumeLiftable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "published", "modified", "опубликовано", "изменено":
		return true
	default:
		return false
	}
}

// --- Key builders ---

// DedupKey hashes account|resume|vacancy into a stable 32-hex-char key for
// the applications.dedup_key UNIQUE column.
func DedupKey(accountID, resumeHash string, vacancyID int64) string {
	sum := sha256.Sum256([]byte(accountID + "|" + resumeHash + "|" + fmt.Sprint(vacancyID)))
	return hex.EncodeToString(sum[:16])
}

// LiftWindow buckets t (UTC, absolute time) into the current 4-hour window
// id ("2026-09-24T08"). Six windows per day cap the daily lifts naturally.
func LiftWindow(t time.Time) string {
	return t.UTC().Truncate(4 * time.Hour).Format("2006-01-02T15")
}

// DayUTC returns the "YYYY-MM-DD" UTC day of t (metrics/quotas).
func DayUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
