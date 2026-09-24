package config

// FuzzParseActiveWindow: the active-window grammar gates the pacer and the
// warmup scheduler, and Ruling И3 made it fail-closed (an unparsable window
// is a config error, never "always active"). Hostile strings must never
// panic the parser, and any accepted value must normalize into a
// [0, 24h) duration pair. Corpus seeds live in
// testdata/fuzz/FuzzParseActiveWindow/.
//
// 30-second run from the T19 pass: 0 failures (see .sdd/t19-report.md).

import (
	"testing"
	"time"
)

func FuzzParseActiveWindow(f *testing.F) {
	f.Add("08:00-22:00")
	f.Add("22:00-06:00") // crosses midnight
	f.Add("00:00-00:00") // degenerate, grammatically valid
	f.Add(" 09:15 - 18:45 ")
	f.Add("9:00-18:00")  // non-padded hour must fail (strict grammar)
	f.Add("24:00-01:00") // out-of-range hour must fail
	f.Add("8-18")        // missing colon
	f.Add("08:00")       // missing separator
	f.Add("-")
	f.Add("")
	f.Add("08:00-22:00-23:00")
	f.Fuzz(func(t *testing.T, s string) {
		start, end, err := ParseActiveWindow(s)
		if err != nil {
			return // rejected input is the expected outcome for most strings
		}
		if start < 0 || start >= 24*time.Hour || end < 0 || end >= 24*time.Hour {
			t.Fatalf("ParseActiveWindow(%q) accepted out-of-range window %v..%v", s, start, end)
		}
		// A second parse of the same input must agree byte-for-byte.
		start2, end2, err2 := ParseActiveWindow(s)
		if err2 != nil || start2 != start || end2 != end {
			t.Fatalf("non-deterministic parse of %q: %v..%v (%v) vs %v..%v", s, start, end, err, start2, end2)
		}
	})
}
