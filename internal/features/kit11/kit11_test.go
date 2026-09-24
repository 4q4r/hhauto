package kit11

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
)

func TestDetectLinks(t *testing.T) {
	in := "Заполните анкету https://docs.google.com/forms/d/abc=view?usp=send_form. Или http://ya.ru, а также www.example.org"
	got := DetectLinks(in)
	want := []string{
		"https://docs.google.com/forms/d/abc=view?usp=send_form",
		"http://ya.ru",
		"www.example.org",
	}
	if len(got) != len(want) {
		t.Fatalf("DetectLinks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DetectLinks[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := DetectLinks("без ссылок, just text"); len(got) != 0 {
		t.Fatalf("DetectLinks(no links) = %v, want empty", got)
	}
}

func TestHasLink(t *testing.T) {
	if !HasLink("смотри https://example.com/x") {
		t.Fatal("HasLink should find https URL")
	}
	if !HasLink("addr: www.example.org") {
		t.Fatal("HasLink should find www URL")
	}
	if HasLink("node.js и 1.2.3.4 не ссылки") {
		t.Fatal("HasLink false positive")
	}
}

func TestLinkHostAndAllowed(t *testing.T) {
	h := LinkHost("https://docs.google.com/forms/d/e/x")
	if h != "docs.google.com" {
		t.Fatalf("LinkHost = %q", h)
	}
	if h := LinkHost("http://WWW.Forms.Yandex.RU/x"); h != "forms.yandex.ru" {
		t.Fatalf("LinkHost lower = %q", h)
	}
	allowed := []string{"forms.gle", "docs.google.com"}
	if !HostAllowed("docs.google.com", allowed) {
		t.Fatal("exact match should pass")
	}
	if !HostAllowed("evil.docs.google.com", allowed) {
		t.Fatal("subdomain match should pass")
	}
	if HostAllowed("notgoogle.com", allowed) || HostAllowed("evildocs.google.com.evil.ru", allowed) {
		t.Fatal("host must not pass suffix check")
	}
	if HostAllowed("", allowed) {
		t.Fatal("empty host must not pass")
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{hhapi.ErrRateLimited, true},
		{hhapi.ErrUpstream, true},
		{hhapi.ErrNetwork, true},
		{hhapi.ErrLoggedOut, false},
		{hhapi.ErrCaptcha, false},
		{hhapi.ErrValidation, false},
		{hhapi.ErrNotFound, false},
		{&hhapi.StatusError{Kind: hhapi.ErrRateLimited}, true},
		{&hhapi.StatusError{Kind: hhapi.ErrValidation}, false},
		{errors.New("boom"), false},
	}
	for i, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Fatalf("case %d: IsTransient(%v) = %v, want %v", i, c.err, got, c.want)
		}
	}
}

func TestIsAmbiguous(t *testing.T) {
	if !IsAmbiguous(&hhapi.AmbiguousError{Err: errors.New("conn reset")}) {
		t.Fatal("AmbiguousError should match")
	}
	if IsAmbiguous(errors.New("other")) || IsAmbiguous(nil) {
		t.Fatal("non-ambiguous must not match")
	}
}

func TestInActiveWindow(t *testing.T) {
	mk := func(h, m int) time.Time {
		return time.Date(2026, 9, 24, h, m, 0, 0, time.Local)
	}
	if !InActiveWindow(mk(10, 0), "08:00-22:00") {
		t.Fatal("10:00 must be inside 08:00-22:00")
	}
	if InActiveWindow(mk(23, 0), "08:00-22:00") {
		t.Fatal("23:00 must be outside 08:00-22:00")
	}
	// boundary: start inclusive, end exclusive
	if !InActiveWindow(mk(8, 0), "08:00-22:00") || InActiveWindow(mk(22, 0), "08:00-22:00") {
		t.Fatal("boundary semantics broken")
	}
	if !InActiveWindow(mk(23, 30), "22:00-06:00") || !InActiveWindow(mk(2, 0), "22:00-06:00") {
		t.Fatal("overnight window must wrap midnight")
	}
	if InActiveWindow(mk(12, 0), "22:00-06:00") {
		t.Fatal("noon must be outside overnight window")
	}
	if !InActiveWindow(mk(3, 0), "") {
		t.Fatal("empty window means always active")
	}
}

func TestIsWeekend(t *testing.T) {
	sat := time.Date(2026, 9, 26, 12, 0, 0, 0, time.Local)
	wed := time.Date(2026, 9, 23, 12, 0, 0, 0, time.Local)
	if !IsWeekend(sat) || IsWeekend(wed) {
		t.Fatal("weekend detection broken")
	}
}

func TestHumanizedDelay(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	lo, hi := 30*time.Minute, 2*time.Hour
	for range 200 {
		d := HumanizedDelay(lo, hi, rnd)
		if d < lo || d > hi {
			t.Fatalf("delay %v outside [%v, %v]", d, lo, hi)
		}
	}
}

func TestBuckets(t *testing.T) {
	ts := time.Date(2026, 9, 24, 15, 4, 0, 0, time.UTC)
	if Day(ts) != "2026-09-24" {
		t.Fatalf("Day = %q", Day(ts))
	}
	if HourKey("emaildup.sent", ts) != "emaildup.sent.h15" {
		t.Fatalf("HourKey = %q", HourKey("emaildup.sent", ts))
	}
}

func TestRenderProfileUsesPromptRendering(t *testing.T) {
	// Wiring guard: RenderProfile must delegate to profile.LoadUserContext and
	// surface its errors (nil store is invalid). Full rendering behavior is
	// covered by internal/profile tests.
	_, err := RenderProfile(t.Context(), config.Config{}, "acc", nil, nil)
	if err == nil {
		t.Fatal("nil db must error")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if !IsUniqueViolation(errors.New("store: MessagesAdd: constraint failed: UNIQUE constraint failed: messages.message_id (2067)")) {
		t.Fatal("unique violation not detected")
	}
	if IsUniqueViolation(errors.New("store: MessagesAdd: some other failure")) {
		t.Fatal("false positive unique violation")
	}
}
