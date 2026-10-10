// Package hhapi is the ONLY place that talks HTTP to hh.ru.
// Endpoint URLs come from config (overridable); nothing else in the codebase
// may hardcode hh.ru URLs. Contracts below are consumed by feature packages.
package hhapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrXSRF extends the contract sentinels (contracts.go stays untouched):
// the server rejected a mutation because the XSRF token was missing/stale.
// The client refreshes the token once and retries; callers may treat it as
// a subcase of ErrForbidden.
var ErrXSRF = errors.New("hhapi: xsrf token rejected")

// ErrTouchTooEarly extends the contract sentinels (T114): hh answered the
// resume touch with 409 because the 4h server timer has not elapsed yet
// (donor-verified semantics: ds8088/hh-resume-auto-boost — status-based
// marker, body not captured live). Autolift plans a short retry cooldown
// instead of counting the touch as done.
var ErrTouchTooEarly = errors.New("hhapi: touch too early")

// StatusError is a typed HTTP failure. errors.Is(err, ErrCaptcha) and the
// other contract sentinels match through Kind.
//
// BodySnippet is diagnostics-only: it is masked by redactSnippet (hh bodies
// can embed _xsrf/hhtoken values) and must never be constructed from raw
// body bytes without it.
type StatusError struct {
	Kind        error         // contract sentinel (ErrRateLimited, ErrCaptcha, ...)
	StatusCode  int           // 0 for non-HTTP (redirect-marker) classifications
	URL         string        // request URL (never contains credentials)
	BodySnippet string        // masked body fragment (see redactSnippet)
	Redirect    string        // Location header when the failure was a redirect
	RetryAfter  time.Duration // honored 429 Retry-After, if any
	Chain       []RedirectHop // redirect chain leading to this response
}

func (e *StatusError) Error() string {
	base := "hhapi: request failed"
	if e.Kind != nil {
		base = "hhapi: " + strings.TrimPrefix(e.Kind.Error(), "hhapi: ")
	}
	if e.StatusCode != 0 {
		base += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.URL != "" {
		base += " " + e.URL
	}
	if e.BodySnippet != "" {
		base += ": " + e.BodySnippet
	}
	return base
}

func (e *StatusError) Unwrap() error { return e.Kind }

// AmbiguousError reports that a non-idempotent mutation was sent but its
// outcome is unknown (connection died mid-flight, 502/504 from the edge).
// The caller must resolve the state by reading (RECONCILE); automatic
// repetition is forbidden — it risks duplicate applications or messages.
type AmbiguousError struct{ Err error }

func (e *AmbiguousError) Error() string {
	return "hhapi: ambiguous mutation outcome (resolve by reading state, do not repeat): " + e.Err.Error()
}

func (e *AmbiguousError) Unwrap() error { return e.Err }

// RedirectHop is one followed redirect: the response that ordered the jump
// and where it pointed.
type RedirectHop struct {
	Status   int
	Location string
}

// Anti-bot / session body signatures. Markers are matched case-insensitively
// against response bodies (and the Location header where noted).
var (
	captchaMarkers = []string{
		"showcaptcha",  // classic hh.ru captcha form anchor
		"/captcha/",    // captcha image/endpoint URLs
		"hhcaptcha",    // hh in-house challenge JSON/page marker
		"g-recaptcha",  // reCAPTCHA widget
		`"isbot":true`, // hhcaptcha/recaptcha JSON verdict (whitespace-stripped before match)
	}
	loginMarkers = []string{
		"/account/login",
		"/account/register",
		"/logon",
	}
	xsrfMarkers = []string{
		"xsrf",
		"csrf",
	}
)

// bodyHasAny reports whether body contains any marker (case-insensitive).
// The body is normalized by removing spaces so JSON markers like
// `"isBot": true` match `"isbot":true`.
func bodyHasAny(body string, markers []string) bool {
	if body == "" {
		return false
	}
	norm := strings.ToLower(strings.ReplaceAll(body, " ", ""))
	for _, m := range markers {
		if strings.Contains(norm, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// redirectKind classifies a redirect target URL: login pages mean the
// session died, captcha pages mean a challenge. Anything else is benign.
func redirectKind(location string) error {
	switch {
	case bodyHasAny(location, loginMarkers):
		return ErrLoggedOut
	case bodyHasAny(location, captchaMarkers):
		return ErrCaptcha
	default:
		return nil
	}
}

// classifyStatus maps an HTTP exchange to a contract sentinel:
// status first, then body signatures, then redirect target signatures.
func classifyStatus(status int, body, location string) error {
	if location != "" {
		if kind := redirectKind(location); kind != nil {
			return kind
		}
	}
	switch {
	case status == 0:
		return ErrNetwork
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusUnauthorized:
		return ErrLoggedOut
	case status == http.StatusForbidden:
		switch {
		case bodyHasAny(body, captchaMarkers):
			return ErrCaptcha
		case bodyHasAny(body, loginMarkers):
			return ErrLoggedOut
		case bodyHasAny(body, xsrfMarkers):
			return ErrXSRF
		default:
			return ErrForbidden
		}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return ErrValidation
	case status == http.StatusNotFound || status == http.StatusGone:
		return ErrNotFound
	case status >= 500:
		return ErrUpstream
	case status >= 300: // redirect with benign target: let caller judge
		return nil
	default:
		return nil
	}
}

// parseRetryAfter parses the Retry-After header (delay-seconds or HTTP-date).
// ok is false when the header is absent or unparseable.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// redact patterns: diagnostics must never carry session credentials. hh
// bodies and URLs can embed _xsrf / hhtoken values in JSON (key: "value"),
// form/query (key=value) and header (key: value) shapes.
var (
	redactJSONRe   = regexp.MustCompile(`(?i)("[^"\[\]{}:]*(?:xsrf|hhtoken|authorization|cookie|token)[^"]*"\s*:\s*)"(?:[^"]*)"`)
	redactAssignRe = regexp.MustCompile(`(?i)((?:^|[&\s,;([])[a-z_.-]*(?:xsrf|hhtoken|authorization|cookie|token)[a-z_.-]*\s*[:=]\s*)(?:("[^"&]*")|[^&\s;,}"]+)`)
)

// redactSnippet masks sensitive token assignments, keeping key names for
// debuggability. Non-sensitive text passes through byte-identical.
func redactSnippet(s string) string {
	s = redactJSONRe.ReplaceAllString(s, `${1}"***"`)
	s = redactAssignRe.ReplaceAllString(s, `${1}***`)
	return s
}

// isDialError reports whether err is a dial-phase network failure: the
// request was never transmitted, so the server state is untouched and the
// failure is NOT ambiguous. Wrapped and joined errors are searched.
func isDialError(err error) bool {
	if err == nil {
		return false
	}
	type opener interface{ Unwrap() []error }
	for e := err; e != nil; e = errors.Unwrap(e) {
		if op, ok := e.(*net.OpError); ok && op.Op == "dial" {
			return true
		}
		if j, ok := e.(opener); ok {
			for _, sub := range j.Unwrap() {
				if isDialError(sub) {
					return true
				}
			}
		}
	}
	return false
}
