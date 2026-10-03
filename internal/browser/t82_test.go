package browser

// T82 identity-parity unit tests: the capture session runs the same
// [browser] preset launch profile as the lanes, its UA gate refuses a
// HeadlessChrome session, and every browser lane pins its identity to the
// captured spec UA (the single source of truth). All pure seams — no
// real Chrome; the live probes live behind build tags.

import (
	"log/slog"
	"strings"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

func t82Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// The capture must launch under the lanes' launch profile: NewFromConfig
// carries the preset and the provider maps it onto the stealth flags.
func TestCaptureProviderCarriesPreset(t *testing.T) {
	p := NewFromConfig(config.Browser{Preset: config.BrowserPresetCareful}, t82Logger())
	flags := p.stealthFlags()
	want, err := StealthFlags(PresetCareful)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != len(want) {
		t.Fatalf("careful capture flags = %v, want %v", flags, want)
	}
	for i := range want {
		if flags[i] != want[i] {
			t.Fatalf("flag %d = %v, want %v", i, flags[i], want[i])
		}
	}
	// Zero-value config keeps the plain launch (no flags).
	if zero := NewFromConfig(config.Browser{}, t82Logger()); len(zero.stealthFlags()) != 0 {
		t.Fatal("zero-value [browser] must keep the plain capture launch")
	}
}

// The capture session UA is stamped into the spec only when it is the
// stealth profile's own — a HeadlessChrome marker fails the capture
// before anything is saved.
func TestValidateSpecUserAgent(t *testing.T) {
	for _, ua := range []string{
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
	} {
		if err := validateSpecUserAgent(ua); err != nil {
			t.Fatalf("validateSpecUserAgent(%q) = %v, want nil", ua, err)
		}
	}
	for _, ua := range []string{
		"",
		"   ",
		"Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/146.0.0.0 Safari/537.36",
	} {
		if err := validateSpecUserAgent(ua); err == nil {
			t.Fatalf("validateSpecUserAgent(%q) = nil, want an error", ua)
		}
	}
}

// Lane UA pin: the spec UA becomes a launch override flag; an empty spec
// UA leaves the lane on its own browser identity.
func TestSpecUserAgentFlags(t *testing.T) {
	if flags := SpecUserAgentFlags(""); flags != nil {
		t.Fatalf("empty spec UA flags = %v, want none", flags)
	}
	ua := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	flags := SpecUserAgentFlags(ua)
	if len(flags) != 1 || flags[0].Name != "user-agent" || flags[0].Value != ua {
		t.Fatalf("flags = %v, want one user-agent flag carrying the spec UA", flags)
	}
}

// LaneUserAgent reads the captured spec UA for a lane: present when the
// spec is usable, empty (fall back to the browser's own identity) when
// the spec is missing or broken — the engine owns the loud recapture.
func TestLaneUserAgent(t *testing.T) {
	dir := t.TempDir()
	if ua := LaneUserAgent(dir, t82Logger()); ua != "" {
		t.Fatalf("missing spec UA = %q, want empty", ua)
	}

	s := &tlsfp.Spec{ // minimal usable identity pair (Load validates these)
		SchemaVersion:     tlsfp.SchemaVersion,
		RawClientHelloHex: "1603010000",
		UserAgent:         "Chrome/146.0.0.0",
		JA3:               "771,4865-4866,0-11-35,29-23-24,0",
		JA3MD5:            "5e8471ca3b95f7e402595b341cfc5bd6",
	}
	path := tlsfp.DefaultPath(dir)
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if ua := LaneUserAgent(dir, t82Logger()); ua != s.UserAgent {
		t.Fatalf("LaneUserAgent = %q, want the spec UA %q", ua, s.UserAgent)
	}

	// A broken spec must not pin a lane to a half identity.
	broken := *s
	broken.JA3MD5 = ""
	if err := broken.Save(path); err != nil {
		t.Fatal(err)
	}
	if ua := LaneUserAgent(dir, t82Logger()); ua != "" || strings.Contains(ua, "Chrome") {
		t.Fatalf("broken spec UA = %q, want empty", ua)
	}
}
