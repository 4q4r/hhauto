package hhapi

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		location string
		want     error
	}{
		{"200 ok", 200, "", "", nil},
		{"204 ok", 204, "", "", nil},
		{"429 rate limited", 429, `{"error":"limit"}`, "", ErrRateLimited},
		{"401 logged out", 401, "", "", ErrLoggedOut},
		{"403 plain forbidden", 403, `{"error":"forbidden"}`, "", ErrForbidden},
		{"403 captcha body", 403, `<form id="showcaptcha">`, "", ErrCaptcha},
		{"403 isbot marker", 403, `{"hhcaptcha":{"isBot":true}}`, "", ErrCaptcha},
		{"403 xsrf marker", 403, `CSRF token expired (xsrf)`, "", ErrXSRF},
		{"400 validation", 400, `{"error":{"required":"letter"}}`, "", ErrValidation},
		{"422 validation", 422, `{"error":"validation"}`, "", ErrValidation},
		{"404 not found", 404, "", "", ErrNotFound},
		{"500 upstream", 500, "oops", "", ErrUpstream},
		{"503 upstream", 503, "", "", ErrUpstream},
		{"501 upstream", 501, "", "", ErrUpstream},
		{"302 login redirect", 302, "", "/account/login?backurl=%2F", ErrLoggedOut},
		{"302 captcha redirect", 302, "", "/captcha/abc", ErrCaptcha},
		{"302 benign redirect", 302, "", "/applicant/resumes", nil},
		{"301 captcha redirect", 301, "", "https://hh.ru/showcaptcha?cc=1", ErrCaptcha},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStatus(tt.status, tt.body, tt.location)
			if !errors.Is(got, tt.want) {
				t.Fatalf("classifyStatus(%d, %q, %q) = %v, want %v", tt.status, tt.body, tt.location, got, tt.want)
			}
		})
	}
}

func TestStatusErrorWrapsKind(t *testing.T) {
	err := &StatusError{Kind: ErrCaptcha, StatusCode: 403, URL: "https://hh.ru/x", BodySnippet: "showcaptcha"}
	if !errors.Is(err, ErrCaptcha) {
		t.Fatal("errors.Is(err, ErrCaptcha) = false, want true")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("Error() = %q, want status code inside", err.Error())
	}
}

func TestAmbiguousErrorUnwrap(t *testing.T) {
	inner := errors.New("EOF")
	err := &AmbiguousError{Err: inner}
	if !errors.Is(err, inner) {
		t.Fatal("errors.Is(ambiguous, inner) = false, want true")
	}
	var target *AmbiguousError
	if !errors.As(err, &target) {
		t.Fatal("errors.As(*AmbiguousError) = false, want true")
	}
	if got := err.Error(); got == "" {
		t.Fatal("Error() empty")
	}
}

func TestBodyHasAny(t *testing.T) {
	if !bodyHasAny(`{"hhcaptcha":{"IsBot":TRUE}}`, captchaMarkers) {
		t.Fatal("case-insensitive captcha marker not found")
	}
	if bodyHasAny(`clean page`, captchaMarkers) {
		t.Fatal("false positive captcha marker")
	}
	if !bodyHasAny(`Please visit /Account/Login`, loginMarkers) {
		t.Fatal("case-insensitive login marker not found")
	}
}

func TestParseRetryAfter(t *testing.T) {
	d, ok := parseRetryAfter("7")
	if !ok || d != 7*time.Second {
		t.Fatalf("parseRetryAfter(\"7\") = %v, %v; want 7s, true", d, ok)
	}
	d, ok = parseRetryAfter("0")
	if !ok || d != 0 {
		t.Fatalf("parseRetryAfter(\"0\") = %v, %v; want 0, true", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Fatal("parseRetryAfter(\"\") ok, want false")
	}
	if _, ok := parseRetryAfter("garbage"); ok {
		t.Fatal("parseRetryAfter(garbage) ok, want false")
	}
	// HTTP-date form must parse and yield a positive delay.
	d, ok = parseRetryAfter(time.Now().UTC().Add(30 * time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	if !ok || d <= 0 {
		t.Fatalf("parseRetryAfter(http-date) = %v, %v; want >0, true", d, ok)
	}
}

func TestIsDialError(t *testing.T) {
	dial := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if !isDialError(dial) {
		t.Fatal("isDialError(dial op) = false, want true")
	}
	if !isDialError(errors.Join(errors.New("wrap"), dial)) {
		t.Fatal("isDialError(joined dial) = false, want true")
	}
	if isDialError(&net.OpError{Op: "read", Err: errors.New("EOF")}) {
		t.Fatal("isDialError(read op) = true, want false")
	}
	if isDialError(errors.New("plain")) {
		t.Fatal("isDialError(plain) = true, want false")
	}
	if isDialError(nil) {
		t.Fatal("isDialError(nil) = true, want false")
	}
}
