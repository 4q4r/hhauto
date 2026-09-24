package featurekit

// T19 fuzz regression: FuzzNoLinksRule found a real panic in NoLinksRule
// (2026-09-24, 30s run). Kept as a skipped RED test so the fix lands with
// a named regression — unskip the body once kit.go is fixed.
//
// Root cause (verified against the panic and a standalone repro):
// NoLinksRule searches the LOWERCASED text, then hands that byte index to
// fragment(text, i), which slices the ORIGINAL text. strings.ToLower grows
// invalid UTF-8 bytes into 3-byte U+FFFD runes, so the index taken from
// `lower` can point past len(text); fragment clamps only `end`, not
// `start`, and s[start:end] panics.
//
// Repro: text = "\xb4\x80\xbe\xb5http://" (11 bytes) →
// len(lower)=19, match at i=12 → fragment slice [12:11] →
// panic: runtime error: slice bounds out of range [12:11]
// (kit.go fragment, called from NoLinksRule).
//
// Suggested one-line fix: search and slice the same string —
// fragment(lower, i) — or clamp start to len(s) as well.
import "testing"

func TestNoLinksRuleInvalidUTF8NoPanic(t *testing.T) {
	// The exact fuzz crasher, kept byte-for-byte:
	text := "\xb4\x80\xbe\xb5http://"
	_ = NoLinksRule(text) // must not panic
	// And it must still report the link (desired behavior after the fix):
	if err := NoLinksRule("https://t.me/x"); err == nil {
		t.Fatal("link not rejected")
	}
}
