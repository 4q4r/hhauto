package autoclone

// T82: the editor lane pins its identity to the captured spec UA — the
// launch flag set is the stealth pair (T32) plus the spec-UA override
// when the wiring carries one.

import (
	"testing"

	"hhauto/internal/browser"
	"hhauto/internal/config"
)

func TestEditorLaunchFlags(t *testing.T) {
	r := &ChromeEditorRunner{opt: EditorOptions{}, log: testLoggerT()}
	if flags := r.launchFlags(); len(flags) != 0 {
		t.Fatalf("zero-config launch flags = %v, want none", flags)
	}

	r.opt.BrowserCfg.Preset = config.BrowserPresetCareful
	r.opt.SpecUserAgent = "Chrome/146.0.0.0"
	flags := r.launchFlags()
	want := 3 // stealth pair + the spec-UA pin
	if len(flags) != want {
		t.Fatalf("launch flags = %v, want %d entries", flags, want)
	}
	last := flags[len(flags)-1]
	if last.Name != "user-agent" || last.Value != "Chrome/146.0.0.0" {
		t.Fatalf("last flag = %+v, want the spec-UA override", last)
	}
	_ = browser.PresetDefault // keep the browser import for future assertions
}
