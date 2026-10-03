package tlsfp

// T82 spec-integrity gates: a cached spec is only usable when it carries
// the complete identity pair — a JA3 fingerprint (the engine's INFO line
// and any drift triage depend on it) and a session UA that is the stealth
// profile's own (never a HeadlessChrome marker). A spec failing these
// checks must fail Load so the engine recaptures it (T50 semantics).

import (
	"strings"
	"testing"
)

func TestLoadRejectsMissingJA3(t *testing.T) {
	s := goldenSpec(t)
	s.JA3 = ""
	s.JA3MD5 = ""
	path := t.TempDir() + "/browser/tls_spec.json"
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, lerr := Load(path); lerr == nil || !strings.Contains(lerr.Error(), "ja3") {
		t.Fatalf("Load without ja3 = %v, want a ja3 integrity error", lerr)
	}
}

func TestLoadRejectsHeadlessChromeUA(t *testing.T) {
	s := goldenSpec(t)
	s.UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/146.0.0.0 Safari/537.36"
	path := t.TempDir() + "/browser/tls_spec.json"
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, lerr := Load(path); lerr == nil || !strings.Contains(lerr.Error(), "HeadlessChrome") {
		t.Fatalf("Load with a HeadlessChrome UA = %v, want a marker error", lerr)
	}
}
