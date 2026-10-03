//go:build live82

package browser

// T82 live probe: what identity does each launch profile present? Two
// sessions of the SAME cached cloakbrowser chromium:
//   - capture-like: the plain chromedp defaults the TLS capture runs with;
//   - lane-like:    the [browser] preset=careful stealth flag set the
//                   editor runner / testsolve filler run with.
// Records navigator.userAgent + navigator.webdriver for both into
// /tmp/opencode/t82/ua_probe.json. Gated: -tags live82 + HHAUTO_LIVE82.
import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

type uaFacts struct {
	UserAgent       string `json:"user_agent"`
	Webdriver       any    `json:"navigator.webdriver"`
	StealthProfile  string `json:"stealth_profile"`
	HeadlessMarker  bool   `json:"has_headless_chrome_marker"`
	ChromiumMarker  bool   `json:"has_chromium_word"`
}

func probeSession(t *testing.T, chromePath string, stealth []StealthFlag) uaFacts {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("no-sandbox", true), // probe-runner-only concern
	)
	for _, f := range stealth {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	ctx, cancel := context.WithTimeout(browserCtx, 60*time.Second)
	defer cancel()

	var ua string
	var wd any
	if err := chromedp.Run(ctx,
		chromedp.Navigate("data:text/html,<title>ok</title>"),
		chromedp.Evaluate(`navigator.userAgent`, &ua),
		chromedp.Evaluate(`navigator.webdriver`, &wd),
	); err != nil {
		t.Skipf("launch needs system chromium deps: %v", err)
	}
	return uaFacts{
		UserAgent:      ua,
		Webdriver:      wd,
		HeadlessMarker: strings.Contains(ua, "HeadlessChrome"),
		ChromiumMarker: strings.Contains(ua, "Chromium/"),
	}
}

func TestLive82IdentityProbe(t *testing.T) {
	if os.Getenv("HHAUTO_LIVE82") == "" {
		t.Skip("live identity probe runs only with HHAUTO_LIVE82 set (and -tags live82)")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("live check pins linux-x64, got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	p := New(Options{}, slog.Default()) // real ~/.cloakbrowser cache, no download
	chromePath, cleanup, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("resolve chrome: %v", err)
	}
	defer cleanup()

	stealth, serr := StealthFlags(PresetCareful)
	if serr != nil {
		t.Fatal(serr)
	}
	out := map[string]uaFacts{
		"capture_like": probeSession(t, chromePath, nil),
		"lane_like":    probeSession(t, chromePath, stealth),
	}
	dir := "/tmp/opencode/t82"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "ua_probe.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("identity probe: %s", b)
}
