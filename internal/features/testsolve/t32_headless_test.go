package testsolve

// T32: the shared browser subsystem wiring in the headless filler — the
// browser.Provider chain (HHAUTO_CHROME_PATH → PATH → cloakbrowser cache →
// auto-download) with cleanup ordering, and the humanize cadence
// primitives (chunked typing plan + per-step fill JS). No real Chrome
// involved: the provider fake proves the wiring; the JS builders are pure.
import (
	"context"
	"strings"
	"testing"

	"hhauto/internal/browser"
	"hhauto/internal/config"
)

// fakeProvider counts Chrome resolutions and cleanups.
type fakeProvider struct {
	calls   int
	cleaned int
	path    string
}

func (f *fakeProvider) Chrome(_ context.Context) (string, func(), error) {
	f.calls++
	return f.path, func() { f.cleaned++ }, nil
}

func TestHeadlessFillerProviderWiring(t *testing.T) {
	fp := &fakeProvider{path: "/nonexistent/fake-chrome"}
	h := &HeadlessFiller{Browser: fp}

	// The fill fails at the chromedp stage (fake executable) — but the
	// provider must have been consulted exactly once and its cleanup must
	// run when the run context is torn down.
	_, err := h.Fill(context.Background(), FillRequest{URL: "data:text/plain,hi"})
	if err == nil {
		t.Fatal("Fill with a fake chrome path must fail at launch")
	}
	if fp.calls != 1 {
		t.Fatalf("provider.Chrome calls = %d, want 1", fp.calls)
	}
	if fp.cleaned != 1 {
		t.Fatalf("provider cleanup runs = %d, want 1 (cancel order)", fp.cleaned)
	}
}

func TestHeadlessFillerLegacyDiscoveryUnchanged(t *testing.T) {
	// nil provider → the legacy h.Chrome override is honored (прежнее
	// поведение).
	used := false
	h := &HeadlessFiller{Chrome: func() (string, error) {
		used = true
		return "", ErrChromeMissing
	}}
	if _, err := h.Fill(context.Background(), FillRequest{URL: "data:text/plain,hi"}); err == nil {
		t.Fatal("legacy discovery failure must error the fill")
	}
	if !used {
		t.Fatal("nil provider must fall back to the legacy Chrome discovery")
	}
}

func TestHeadlessFillerHumanizeToggle(t *testing.T) {
	var h HeadlessFiller
	if h.humanize() {
		t.Fatal("zero-value config keeps the legacy single-pass fill")
	}
	on := true
	h.Cfg.Browser.Humanize = &on
	if !h.humanize() {
		t.Fatal("explicit humanize=true must enable the cadence path")
	}
}

func TestTypePlanSteps(t *testing.T) {
	pacer, err := browser.NewPacer(browser.PresetCareful, 7)
	if err != nil {
		t.Fatal(err)
	}
	ops := []fillOp{
		{Kind: "set", Ctrls: []int{0}, Value: "Привет, world"},
		{Kind: "check", Ctrls: []int{1}, Checked: true},
	}
	steps := typePlanSteps(ops, pacer)

	// "Привет, world" = 13 UTF-16 units, careful chunk 3, cap 160 →
	// 5 typed chunks (3+3+3+3+1), then one single step for the checkbox.
	typed := 0
	for _, s := range steps {
		if !s.typed {
			continue
		}
		typed++
		if s.first && s.chunk.From != 0 {
			t.Fatalf("first chunk From = %d, want 0", s.chunk.From)
		}
	}
	if typed != 5 {
		t.Fatalf("typed chunks = %d, want 5", typed)
	}
	lastTyped := steps[len(steps)-2]
	if !lastTyped.last || lastTyped.chunk.To != 13 {
		t.Fatalf("last text chunk = %+v, want To 13 with last=true", lastTyped)
	}
	tail := steps[len(steps)-1]
	if tail.typed || tail.op.Kind != "check" {
		t.Fatalf("tail step = %+v, want the single non-text op", tail)
	}
}

func TestHumanFillJSChunks(t *testing.T) {
	op := fillOp{Kind: "set", Ctrls: []int{2}, Value: "abcdef"}
	js := humanFillJS(humanStep{op: op, chunk: browser.TypeChunk{From: 0, To: 6}, first: true, last: true, typed: true})
	for _, want := range []string{
		"el.focus()",            // smooth focus on the first chunk
		"const to = 6;",         // the chunk boundary drives the slice
		"op.value.slice(0, to)", // native setter slice
		"['input', 'change']",   // full event set on the final chunk
		`"kind":"set"`,          // the op travels with the step
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("humanFillJS missing %q in:\n%s", want, js)
		}
	}

	// The last flag is interpolated into the step, so a mid-chunk
	// (last=false) takes the input-only branch at runtime.
	mid := humanFillJS(humanStep{op: op, chunk: browser.TypeChunk{From: 0, To: 3}, first: true, last: false, typed: true})
	if !strings.Contains(mid, "last = false") || !strings.Contains(mid, "fire(el, ['input'])") {
		t.Fatal("a mid-chunk must carry last=false and the input-only branch")
	}

	radio := humanFillJS(humanStep{op: fillOp{Kind: "radio", Ctrls: []int{4}}})
	if !strings.Contains(radio, "typed = false") || !strings.Contains(radio, "op.kind === 'radio'") {
		t.Fatalf("radio step must be a non-typed radio op: %s", radio)
	}
}

func TestHeadlessFillerStealthFlagsFromConfig(t *testing.T) {
	h := &HeadlessFiller{Cfg: config.Config{}} // zero config → no flags (прежнее поведение)
	if flags := h.stealthFlags(); len(flags) != 0 {
		t.Fatalf("zero-config flags = %v, want none", flags)
	}
	h.Cfg.Browser.Preset = config.BrowserPresetCareful
	flags := h.stealthFlags()
	if len(flags) != 2 {
		t.Fatalf("careful flags = %v, want the stealth pair", flags)
	}
	if flags[0].Name != "disable-blink-features" || flags[1].Name != "enable-automation" {
		t.Fatalf("flags = %v, want disable-blink-features + enable-automation", flags)
	}
}
