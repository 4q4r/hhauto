// Package kit11 is the minimal shared helper set for the T11 feature
// packages (chatops, testsolve, interview, contacts, warmup, emaildup).
// It is intentionally separate from other feature-helper packages so the
// parallel Wave-3 tasks never collide; the orchestrator may consolidate
// overlapping helpers at merge time. Pure functions and small adapters only.
package kit11

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// APIResolver returns the per-account hh API client. Engine wiring (T13)
// provides the real resolver; tests provide fakes.
type APIResolver func(accountID string) (hhapi.HHAPI, error)

var linkRe = regexp.MustCompile(`(?:https?://[^\s<>"')\]]+|www\.[A-Za-z0-9-]+(?:\.[^\s<>"')\]]+)?)`)

// trimEnd strips trailing punctuation that is almost certainly sentence
// punctuation, not part of the URL.
func trimEnd(s string) string {
	return strings.TrimRight(s, ".,;:!?)\u00bb\"'")
}

// DetectLinks extracts URL candidates from free text: explicit http(s) URLs
// and www.-prefixed hosts. Order-preserving, deduplicated. Bare domains
// without a scheme or www. prefix are deliberately NOT detected (false
// positives: "node.js", version numbers).
func DetectLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range linkRe.FindAllString(text, -1) {
		m = trimEnd(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// HasLink reports whether outgoing text contains any URL. This backs the
// platform rule "no links in outgoing messages" (shadow-ban risk).
func HasLink(text string) bool { return len(DetectLinks(text)) > 0 }

// LinkHost returns the lowercased host of a detected link. www. is stripped;
// a www.-link without scheme is normalized to http:// for parsing. An empty
// string is returned when the host cannot be parsed.
func LinkHost(raw string) string {
	s := raw
	if strings.HasPrefix(s, "www.") {
		s = "http://" + s
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 { // userinfo must never count as host
		s = s[i+1:]
	}
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || !strings.Contains(s, ".") {
		return ""
	}
	return strings.TrimPrefix(s, "www.")
}

// HostAllowed reports whether host equals an allowed domain or is a direct
// subdomain of one. An empty allowed list permits nothing.
func HostAllowed(host string, allowed []string) bool {
	if host == "" {
		return false
	}
	for _, d := range allowed {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// IsTransient classifies hh API failures that are worth retrying later:
// rate limit, upstream 5xx, network. Everything else (captcha, logout,
// validation, unknown) is treated as non-transient — the caller records
// it instead of looping.
func IsTransient(err error) bool {
	return err != nil && (errors.Is(err, hhapi.ErrRateLimited) ||
		errors.Is(err, hhapi.ErrUpstream) ||
		errors.Is(err, hhapi.ErrNetwork))
}

// IsAmbiguous reports whether err is an hhapi.AmbiguousError: a mutation was
// sent but its outcome is unknown. Callers must RECONCILE by reading state;
// blind repetition is forbidden.
func IsAmbiguous(err error) bool {
	var ae *hhapi.AmbiguousError
	return errors.As(err, &ae)
}

// IsUniqueViolation reports whether err is a UNIQUE-constraint failure from
// the store (idempotent-insert race: the row already exists).
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// InActiveWindow reports whether local time t falls into "HH:MM-HH:MM".
// The window may wrap midnight ("22:00-06:00"). An empty window means
// "always active". Start is inclusive, end exclusive.
func InActiveWindow(t time.Time, window string) bool {
	window = strings.TrimSpace(window)
	if window == "" {
		return true
	}
	parts := strings.SplitN(window, "-", 2)
	if len(parts) != 2 {
		return true
	}
	start, err1 := time.Parse("15:04", strings.TrimSpace(parts[0]))
	end, err2 := time.Parse("15:04", strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return true
	}
	cur := t.Hour()*60 + t.Minute()
	from := start.Hour()*60 + start.Minute()
	to := end.Hour()*60 + end.Minute()
	if from == to {
		return true
	}
	if from < to {
		return cur >= from && cur < to
	}
	return cur >= from || cur < to // overnight wrap
}

// IsWeekend reports whether local time t is Saturday or Sunday.
func IsWeekend(t time.Time) bool {
	return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
}

// HumanizedDelay returns a random duration in [min, max] (inclusive bounds
// on the second granularity) used to disguise mechanical timing.
func HumanizedDelay(min, max time.Duration, rnd *rand.Rand) time.Duration {
	if max <= min {
		return min
	}
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	}
	return min + time.Duration(rnd.Int64N(int64(max-min)+int64(time.Second)))
}

// Day renders the UTC day bucket ("YYYY-MM-DD") used by MetricsIncr.
func Day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// HourKey renders an intra-day metric key: "<prefix>.h<HH>" (UTC hour).
// Metrics are stored per (key, day), so an hour bucket inside the key gives
// hourly counters without schema changes.
func HourKey(prefix string, t time.Time) string {
	return fmt.Sprintf("%s.h%02d", prefix, t.UTC().Hour())
}

// RenderProfile builds the trusted candidate-context prompt block for an
// account (resume snapshot + profile.md + stored facts) via internal/profile.
func RenderProfile(ctx context.Context, cfg config.Config, accountID string, db *store.DB, api hhapi.HHAPI) (string, error) {
	p, err := profile.LoadUserContext(ctx, cfg, accountID, db, api)
	if err != nil {
		return "", fmt.Errorf("kit11: render profile for %s: %w", accountID, err)
	}
	return profile.RenderForPrompt(p), nil
}
