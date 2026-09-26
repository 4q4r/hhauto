package browser

// T32 unit tests: the Chrome resolution chain, the cloakbrowser cache
// formats and the auto-download flow (fake HTTP server), plus the
// humanize/stealth launch primitives. Everything runs without a real
// Chrome; the live download test lives behind the live32 build tag.
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"hhauto/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func execFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// countingServer wraps a handler and counts requests.
type countingServer struct {
	*httptest.Server
	requests atomic.Int64
}

func newCountingServer(t *testing.T, h http.HandlerFunc) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.requests.Add(1)
		h(w, r)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func TestProviderEnvPathWins(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PATH/env layout assertions are linux-specific")
	}
	dir := t.TempDir()
	want := execFile(t, dir, "my-chrome")
	t.Setenv("HHAUTO_CHROME_PATH", want)

	p := New(Options{CacheDir: t.TempDir()}, quietLogger())
	got, cleanup, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	defer cleanup()
	if got != want {
		t.Fatalf("Chrome = %q, want %q", got, want)
	}
}

func TestProviderEnvPathBrokenIsHardError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PATH/env layout assertions are linux-specific")
	}
	t.Setenv("HHAUTO_CHROME_PATH", "/nonexistent/chrome-xyz")

	p := New(Options{AutoDownload: true, CacheDir: t.TempDir()}, quietLogger())
	_, _, err := p.Chrome(context.Background())
	if err == nil {
		t.Fatal("a set-but-broken HHAUTO_CHROME_PATH must be a hard error (explicit setting is the source of truth)")
	}
}

func TestProviderPATHFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PATH/env layout assertions are linux-specific")
	}
	t.Setenv("HHAUTO_CHROME_PATH", "")
	bin := t.TempDir()
	execFile(t, bin, "chromium")
	t.Setenv("PATH", bin)

	p := New(Options{CacheDir: t.TempDir()}, quietLogger())
	got, cleanup, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	defer cleanup()
	if got != filepath.Join(bin, "chromium") {
		t.Fatalf("Chrome = %q, want the PATH hit", got)
	}
}

func TestProviderCacheHit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cache layout assertions are linux-specific")
	}
	srv := newCountingServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP request on cache hit: %s", r.URL)
	})
	cache := t.TempDir()
	ver := "146.0.7680.177.5"
	execFile(t, filepath.Join(cache, "chromium-"+ver), "chrome")

	p := New(Options{CacheDir: cache, DownloadURL: srv.URL}, quietLogger())
	got, cleanup, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	defer cleanup()
	if got != filepath.Join(cache, "chromium-"+ver, "chrome") {
		t.Fatalf("Chrome = %q, want the cached binary", got)
	}
	if n := srv.requests.Load(); n != 0 {
		t.Fatalf("cache hit made %d HTTP requests, want 0", n)
	}
}

func TestProviderCacheMarkerNewerWins(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cache layout assertions are linux-specific")
	}
	cache := t.TempDir()
	newer := "147.0.0.0.1"
	execFile(t, filepath.Join(cache, "chromium-"+newer), "chrome")
	if err := os.WriteFile(filepath.Join(cache, "latest_version_linux-x64"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(Options{CacheDir: cache, Channel: ChannelAuto}, quietLogger())
	got, _, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	if !strings.Contains(got, "chromium-"+newer) {
		t.Fatalf("Chrome = %q, want the marker version %q", got, newer)
	}
}

func TestProviderCacheFreeIgnoresMarker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cache layout assertions are linux-specific")
	}
	cache := t.TempDir()
	pinned := "146.0.7680.177.5"
	execFile(t, filepath.Join(cache, "chromium-"+pinned), "chrome")
	if err := os.WriteFile(filepath.Join(cache, "latest_version_linux-x64"), []byte("147.0.0.0.1"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(Options{CacheDir: cache, Channel: ChannelFree}, quietLogger())
	got, _, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	if !strings.Contains(got, "chromium-"+pinned) {
		t.Fatalf("Chrome = %q, channel free must ignore the update marker", got)
	}
}

// fakeCloakServer serves a minimal cloakbrowser release: SHA256SUMS plus a
// tar.gz archive holding a fake `chrome` executable.
func fakeCloakServer(t *testing.T, chromeScript string, badSums bool) *countingServer {
	t.Helper()
	archive := tarGz(t, map[string]tarEntry{
		"chrome":     {Content: chromeScript, Mode: 0o755},
		"icudtl.dat": {Content: "data", Mode: 0o644},
	})
	sums := sha256.Sum256(archive)
	if badSums {
		sums = sha256.Sum256([]byte("not-the-archive"))
	}
	name := "cloakbrowser-linux-x64.tar.gz"
	sumsBody := hex.EncodeToString(sums[:]) + "  " + name + "\n"

	return newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			_, _ = io.WriteString(w, sumsBody)
		case name:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	})
}

type tarEntry struct {
	Content string
	Mode    int64
}

func tarGz(t *testing.T, files map[string]tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: f.Mode, Size: int64(len(f.Content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.Content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProviderDownloadFlow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("download layout assertions are linux-specific")
	}
	version := "146.0.7680.177.5"
	srv := fakeCloakServer(t, "#!/bin/sh\nexit 0\n", false)
	cache := t.TempDir()

	p := New(Options{
		AutoDownload: true,
		CacheDir:     cache,
		DownloadURL:  srv.URL,
	}, quietLogger())
	got, cleanup, err := p.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	want := filepath.Join(cache, "chromium-"+version, "chrome")
	if got != want {
		t.Fatalf("Chrome = %q, want %q", got, want)
	}
	// The binary is executable and the extraction landed both files.
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o100 == 0 {
		t.Fatal("downloaded binary must carry the executable bit")
	}
	if _, err := os.Stat(filepath.Join(cache, "chromium-"+version, "icudtl.dat")); err != nil {
		t.Fatalf("archive extraction lost a file: %v", err)
	}
	// The auto-update marker is written for the next run (upstream layout).
	if b, err := os.ReadFile(filepath.Join(cache, "latest_version_linux-x64")); err != nil || string(b) != version {
		t.Fatalf("marker = %q, %v; want %q", b, err, version)
	}
	// cleanup is a no-op for cache-installed binaries (the cache persists).
	cleanup()
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("cleanup removed the cached binary: %v", err)
	}
	// The second resolution is a cache hit (no new requests beyond the
	// first call's SHA256SUMS + archive).
	before := srv.requests.Load()
	if got2, _, err := p.Chrome(context.Background()); err != nil || got2 != want {
		t.Fatalf("second Chrome = %q, %v; want %q", got2, err, want)
	}
	if after := srv.requests.Load(); after != before {
		t.Fatalf("second Chrome made %d new requests, want 0 (memoized)", after-before)
	}
}

func TestProviderDownloadChecksumMismatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("download layout assertions are linux-specific")
	}
	version := "146.0.7680.177.5"
	srv := fakeCloakServer(t, "#!/bin/sh\nexit 0\n", true)
	cache := t.TempDir()

	p := New(Options{AutoDownload: true, CacheDir: cache, DownloadURL: srv.URL}, quietLogger())
	_, _, err := p.Chrome(context.Background())
	if err == nil || !strings.Contains(err.Error(), "контрольн") && !strings.Contains(err.Error(), "checksum") && !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("Chrome err = %v, want a checksum mismatch error", err)
	}
	if _, serr := os.Stat(filepath.Join(cache, "chromium-"+version)); !os.IsNotExist(serr) {
		t.Fatalf("a failed download must not leave the version dir: %v", serr)
	}
}

func TestProviderNoAutoDownload(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PATH/env layout assertions are linux-specific")
	}
	t.Setenv("HHAUTO_CHROME_PATH", "")
	t.Setenv("PATH", t.TempDir()) // no chrome anywhere

	p := New(Options{AutoDownload: false, CacheDir: t.TempDir()}, quietLogger())
	_, _, err := p.Chrome(context.Background())
	if err == nil {
		t.Fatal("auto_download=false with empty cache must fail with ErrChromeMissing")
	}
}

func TestProviderProChannelCacheOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cache layout assertions are linux-specific")
	}
	srv := newCountingServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("pro channel must not download in v1: %s", r.URL)
	})
	cache := t.TempDir()
	p := New(Options{CacheDir: cache, Channel: ChannelPro, DownloadURL: srv.URL}, quietLogger())
	if _, _, err := p.Chrome(context.Background()); err == nil || !strings.Contains(err.Error(), ChannelPro) {
		t.Fatalf("Chrome err = %v, want a pro-channel explanation", err)
	}

	// A cached pro build resolves — through a FRESH provider (production
	// builds the provider once at startup, after any cache exists; the
	// first instance legitimately memoized the cache-miss error).
	proVer := "151.0.7922.108.2"
	execFile(t, filepath.Join(cache, "chromium-"+proVer+"-pro"), "chrome")
	if err := os.WriteFile(filepath.Join(cache, "latest_pro_version_linux-x64"), []byte(proVer), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(Options{CacheDir: cache, Channel: ChannelPro, DownloadURL: srv.URL}, quietLogger())
	got, _, err := p2.Chrome(context.Background())
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	if !strings.Contains(got, proVer+"-pro") {
		t.Fatalf("Chrome = %q, want the pro build", got)
	}
}

func TestStealthFlags(t *testing.T) {
	careful, err := StealthFlags(PresetCareful)
	if err != nil {
		t.Fatal(err)
	}
	joined := make([]string, 0, len(careful))
	for _, f := range careful {
		joined = append(joined, f.Name)
	}
	if !contains(joined, "disable-blink-features") || !contains(joined, "enable-automation") {
		t.Fatalf("careful flags = %v, want the AutomationControlled pair", careful)
	}
	for _, f := range careful {
		if f.Name == "disable-blink-features" && f.Value != "AutomationControlled" {
			t.Fatalf("disable-blink-features = %v, want AutomationControlled", f.Value)
		}
		if f.Name == "enable-automation" && f.Value != false {
			t.Fatalf("enable-automation = %v, want false (drop the automation switch)", f.Value)
		}
	}
	if def, err := StealthFlags(PresetDefault); err != nil || len(def) != 0 {
		t.Fatalf("default preset flags = %v, %v; want none", def, err)
	}
	if _, err := StealthFlags("aggressive"); err == nil {
		t.Fatal("unknown preset must error")
	}
}

func TestPacerBoundsAndContext(t *testing.T) {
	p, err := NewPacer(PresetCareful, 42)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if d := p.ActionDelay(); d < 60*time.Millisecond || d > 180*time.Millisecond {
			t.Fatalf("careful action delay %v outside [60ms, 180ms]", d)
		}
		if d := p.TypeDelay(); d < 30*time.Millisecond || d > 90*time.Millisecond {
			t.Fatalf("careful type delay %v outside [30ms, 90ms]", d)
		}
	}
	def, _ := NewPacer(PresetDefault, 42)
	if def.ActionDelay() > p.ActionDelay() {
		t.Fatal("careful pacing must be slower than default")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx, PaceAction); err == nil {
		t.Fatal("Wait must honor a canceled context")
	}
}

func TestLookPathNames(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PATH layout assertions are linux-specific")
	}
	bin := t.TempDir()
	execFile(t, bin, "google-chrome-stable")
	t.Setenv("HHAUTO_CHROME_PATH", "")
	t.Setenv("PATH", bin)
	got, ok := lookPath("")
	if !ok || got != filepath.Join(bin, "google-chrome-stable") {
		t.Fatalf("lookPath = %q, %v; want google-chrome-stable", got, ok)
	}

	// A PATH without any chrome-family name must not resolve.
	t.Setenv("PATH", t.TempDir())
	if got, ok := lookPath(""); ok {
		t.Fatalf("lookPath = %q on a PATH without chrome, want miss", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestTypePlan(t *testing.T) {
	// "Привет, world" = 13 UTF-16 units; chunk 3, cap 6 → two typed
	// chunks of 3 units, then the remainder collapses into one batch.
	plan := TypePlan("Привет, world", 3, 6)
	if len(plan) != 3 {
		t.Fatalf("TypePlan chunks = %d, want 3 (%v)", len(plan), plan)
	}
	want := []TypeChunk{{From: 0, To: 3}, {From: 3, To: 6}, {From: 6, To: 13}}
	for i, c := range plan {
		if c != want[i] {
			t.Fatalf("chunk %d = %v, want %v", i, c, want[i])
		}
	}

	// A short value is one chunk; an empty value plans nothing.
	if plan := TypePlan("ab", 3, 6); len(plan) != 1 || plan[0] != (TypeChunk{From: 0, To: 2}) {
		t.Fatalf("TypePlan(short) = %v, want one [0,2) chunk", plan)
	}
	if plan := TypePlan("", 3, 6); len(plan) != 0 {
		t.Fatalf("TypePlan(empty) = %v, want none", plan)
	}

	// The cap always covers the whole value: the end state is identical
	// to a single native set.
	for _, value := range []string{"x", "тестовая строка", "1234567890"} {
		plan := TypePlan(value, 2, 4)
		if plan[0].From != 0 || plan[len(plan)-1].To != len(utf16Encode(value)) {
			t.Fatalf("TypePlan(%q) = %v, must cover the full value", value, plan)
		}
	}
}

func utf16Encode(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

func TestNewFromConfig(t *testing.T) {
	if p := NewFromConfig(config.Browser{}, quietLogger()); p.opt.AutoDownload {
		t.Fatal("zero-value config keeps auto_download off (defaults activate it)")
	}
	on := true
	p := NewFromConfig(config.Browser{AutoDownload: &on, Channel: config.BrowserChannelFree}, quietLogger())
	if !p.opt.AutoDownload || p.opt.Channel != ChannelFree {
		t.Fatalf("options = %+v, want auto_download on + free channel", p.opt)
	}
}
