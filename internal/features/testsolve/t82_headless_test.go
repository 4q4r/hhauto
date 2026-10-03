package testsolve

// T82: the testsolve headless lane pins its identity to the captured
// spec UA — the launch flag set is the stealth pair (T32) plus the
// spec-UA override when the spec is usable.

import (
	"log/slog"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

func TestHeadlessFillerLaunchFlags(t *testing.T) {
	h := &HeadlessFiller{Cfg: config.Config{}, Log: slog.New(slog.DiscardHandler)}
	if flags := h.launchFlags(); len(flags) != 0 {
		t.Fatalf("zero-config launch flags = %v, want none", flags)
	}

	h.Cfg.Browser.Preset = config.BrowserPresetCareful
	if flags := h.launchFlags(); len(flags) != 2 {
		t.Fatalf("stealth-only flags = %v, want the stealth pair (no usable spec in a zero config)", flags)
	}

	// A usable capture under the config data dir pins the lane's UA.
	dir := t.TempDir()
	spec := &tlsfp.Spec{
		SchemaVersion:     tlsfp.SchemaVersion,
		RawClientHelloHex: "1603010000",
		UserAgent:         "Chrome/146.0.0.0",
		JA3:               "771,4865-4866,0-11-35,29-23-24,0",
		JA3MD5:            "5e8471ca3b95f7e402595b341cfc5bd6",
	}
	if err := spec.Save(tlsfp.DefaultPath(dir)); err != nil {
		t.Fatal(err)
	}
	h.Cfg.General.DataDir = dir
	flags := h.launchFlags()
	if len(flags) != 3 {
		t.Fatalf("launch flags = %v, want the stealth pair + the spec-UA pin", flags)
	}
	last := flags[len(flags)-1]
	if last.Name != "user-agent" || last.Value != "Chrome/146.0.0.0" {
		t.Fatalf("last flag = %+v, want the spec-UA override", last)
	}
}
