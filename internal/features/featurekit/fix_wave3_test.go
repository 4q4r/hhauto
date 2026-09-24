package featurekit

import "testing"

// FIX 6: the single shared published-status helper used by autolift and
// multiapply (hh surfaces either the dictionary id or the display name).
func TestResumeLiftable(t *testing.T) {
	cases := map[string]bool{
		"published":     true,
		"PUBLISHED":     true,
		"Опубликовано":  true,
		"опубликовано":  true,
		"  published  ": true,
		"draft":         false,
		"черновик":      false,
		"":              false,
	}
	for status, want := range cases {
		if got := ResumeLiftable(status); got != want {
			t.Fatalf("ResumeLiftable(%q) = %v, want %v", status, got, want)
		}
	}
}
