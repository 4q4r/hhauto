package autoclone

// T32: the shared browser subsystem wiring in the editor runner — the
// browser.Provider chain inside startChrome (with cleanup ordering), the
// careful-preset stealth flags and the humanize cadence primitives
// (typed-op split, chunked set JS, pacer construction). The chromedp
// launch itself needs no real Chrome for these assertions: a fake
// executable path fails fast and exercises the full teardown path.
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"hhauto/internal/config"
)

// countingChromeProvider is a browser.Provider fake.
type countingChromeProvider struct {
	calls   int
	cleaned int
	path    string
}

func (f *countingChromeProvider) Chrome(_ context.Context) (string, func(), error) {
	f.calls++
	return f.path, func() { f.cleaned++ }, nil
}

func TestEditorChromeProviderWiring(t *testing.T) {
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	provider := &countingChromeProvider{path: "/nonexistent/fake-chrome"}

	r, err := NewChromeEditorRunner(EditorOptions{
		Cfg:            editorCfg("https://hh.ru"),
		DB:             db,
		Cookies:        &fakeCookieSource{byAccount: map[string][]*http.Cookie{"acc1": testSessionCookie()}},
		Verify:         newFakeEditor(t),
		ChromeProvider: provider,
		Log:            testLoggerT(),
	})
	if err != nil {
		t.Fatalf("NewChromeEditorRunner: %v", err)
	}
	_, err = r.ApplyFieldPlan(context.Background(), "clone1", testPlan())
	if !errors.Is(err, ErrEditorUnavailable) {
		t.Fatalf("err = %v, want ErrEditorUnavailable", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider.Chrome calls = %d, want 1", provider.calls)
	}
	if provider.cleaned != 1 {
		t.Fatalf("provider cleanup runs = %d, want 1 (teardown order)", provider.cleaned)
	}
}

func TestSplitTypedOps(t *testing.T) {
	ops := []editorFillOp{
		{Field: "a", Value: "text"},
		{Field: "b", Selectors: []string{"[data-qa=b]"}},
		{Field: "c", Value: "more"},
	}
	typed, rest := splitTypedOps(ops)
	if len(typed) != 2 || typed[0].Field != "a" || typed[1].Field != "c" {
		t.Fatalf("typed = %v, want a,c in order", typed)
	}
	if len(rest) != 1 || rest[0].Field != "b" {
		t.Fatalf("rest = %v, want b", rest)
	}
}

func TestHumanEditorSetJS(t *testing.T) {
	op := editorFillOp{Field: "resume.title", Label: "Заголовок", Selectors: []string{"[data-qa=title]"}, Value: "abcdef"}
	js := humanEditorSetJS(op, 6, true, true)
	for _, want := range []string{
		"hit.el.focus()",                        // smooth focus on the first chunk
		"const to = 6,",                         // the chunk boundary drives the slice
		"op.value.slice(0, to)",                 // native setter slice
		"'input', 'change', 'blur', 'focusout'", // full event set on the final chunk
		`"field":"resume.title"`,                // the op travels with the step
		"bySelectors(op.selectors)",             // same resolution as the batched fill
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("humanEditorSetJS missing %q in:\n%s", want, js)
		}
	}
	// The payload must decode as one outcome.
	raw := `{"field":"f","ok":true,"selector":"x"}`
	var oc editorOpOutcome
	if err := json.Unmarshal([]byte(raw), &oc); err != nil || !oc.OK {
		t.Fatalf("outcome decode: %v %+v", err, oc)
	}
}

func TestEditorPacer(t *testing.T) {
	if p := editorPacer(config.Browser{}); p != nil {
		t.Fatal("zero-value [browser] keeps the legacy pacing (nil pacer)")
	}
	on := true
	if p := editorPacer(config.Browser{Humanize: &on, Preset: config.BrowserPresetCareful}); p == nil {
		t.Fatal("humanize=true must build a pacer")
	}
}

func TestEditorStealthFlags(t *testing.T) {
	r := &ChromeEditorRunner{opt: EditorOptions{}, log: testLoggerT()}
	if flags := r.stealthFlags(); len(flags) != 0 {
		t.Fatalf("zero-config flags = %v, want none", flags)
	}
	r.opt.BrowserCfg.Preset = config.BrowserPresetCareful
	flags := r.stealthFlags()
	if len(flags) != 2 || flags[0].Name != "disable-blink-features" || flags[1].Name != "enable-automation" {
		t.Fatalf("careful flags = %v, want the stealth pair", flags)
	}
}
