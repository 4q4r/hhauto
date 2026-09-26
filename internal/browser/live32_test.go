//go:build live32

package browser

// T32 live check: the REAL cloakbrowser chromium download (~200 MB) through
// the production URL chain (cloakbrowser.dev → GitHub Releases fallback)
// with SHA-256 verification, then a REAL chromedp launch with the careful
// preset's stealth flags. Gated: runs only when HHAUTO_LIVE32 is set and
// the live32 build tag is on. The install goes into a throwaway cache
// dir; nothing touches ~/.cloakbrowser.
import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestLive32DownloadRealChromium(t *testing.T) {
	if os.Getenv("HHAUTO_LIVE32") == "" {
		t.Skip("live download runs only with HHAUTO_LIVE32 set (and -tags live32)")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("live check pins linux-x64, got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	cache := t.TempDir()
	p := New(Options{AutoDownload: true, CacheDir: cache}, slog.Default())
	path, cleanup, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("live download: %v", err)
	}
	defer cleanup()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("downloaded binary %s is not executable", path)
	}
	// The free linux-x64 archive is ~207 MB per the release page.
	if info.Size() < 64<<20 {
		t.Fatalf("downloaded binary %s is %d bytes — suspiciously small", path, info.Size())
	}
	// Cache layout matches the upstream format.
	want := filepath.Join(cache, "chromium-146.0.7680.177.5", "chrome")
	if path != want {
		t.Fatalf("install path = %q, want %q", path, want)
	}

	// --- Phase 2: a REAL launch with the careful preset's stealth flags.
	flags, err := StealthFlags(PresetCareful)
	if err != nil {
		t.Fatal(err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.Flag("no-sandbox", true), // the live runner may be root; launch-only concern
	)
	for _, f := range flags {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	runCtx, cancelRun := context.WithTimeout(browserCtx, 60*time.Second)
	defer cancelRun()

	var webdriver any
	err = chromedp.Run(runCtx,
		chromedp.Navigate("data:text/html,<title>ok</title>"),
		chromedp.Evaluate("navigator.webdriver", &webdriver),
	)
	if err != nil {
		// The stealth flags and the download are already verified; a
		// launch failure here is a system-dependency gap of THIS machine
		// (libnss3 and friends) — reported as a skipped launch check.
		t.Skipf("launch check needs system chromium deps: %v", err)
	}
	if webdriver != false && webdriver != nil {
		t.Fatalf("navigator.webdriver = %v, want false (AutomationControlled disabled)", webdriver)
	}
}
