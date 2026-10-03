// Package browser is the single Chrome entry point for hhauto's browser
// consumers (T32): the testsolve headless filler and the autoclone editor
// runner. Provider.Chrome resolves an executable through the chain
//
//	HHAUTO_CHROME_PATH → PATH → ~/.cloakbrowser cache → auto-download
//
// where the auto-download mirrors the CloakHQ/CloakBrowser release layout
// (https://github.com/CloakHQ/CloakBrowser/blob/main/cloakbrowser/config.py):
// cache dir ~/.cloakbrowser, binary dirs chromium-<version>[-pro], update
// markers latest_version_<platform-tag> / latest_pro_version_<platform-tag>,
// archives cloakbrowser-<tag>.tar.gz(.zip) under
// https://cloakbrowser.dev/chromium-v<ver>/ with the
// github.com/CloakHQ/cloakbrowser releases as fallback. SHA-256 checksums
// are verified when the source publishes SHA256SUMS.
//
// The launch-profile half of the package is the humanize/stealth layer:
// StealthFlags maps the config preset onto conservative Chromium launch
// flags and Pacer provides the humanize action pacing (the typing cadence
// itself lives in the consumers, which own their fill JS).
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"hhauto/internal/config"
)

// Release-channel and humanize-preset vocabularies (shared with [browser]
// in internal/config — single source of truth).
const (
	PresetDefault = config.BrowserPresetDefault
	PresetCareful = config.BrowserPresetCareful

	ChannelAuto = config.BrowserChannelAuto
	ChannelFree = config.BrowserChannelFree
	ChannelPro  = config.BrowserChannelPro
)

// ErrChromeMissing reports that the whole resolution chain failed: no
// HHAUTO_CHROME_PATH, no chrome-family name on PATH, no compatible cache
// entry and (when auto_download is off) no download attempt.
var ErrChromeMissing = errors.New("browser: Chrome не найден (HHAUTO_CHROME_PATH/PATH пусты; установите chromium или включите browser.auto_download)")

// chromeNames is the PATH candidate list — identical to the legacy
// testsolve/autoclone discovery.
var chromeNames = []string{"chrome", "chromium", "google-chrome", "google-chrome-stable", "chromium-browser"}

// Provider resolves the browser executable for one consumer. cleanup is
// always safe to call and currently a no-op (downloaded binaries persist
// in the cache by design); the signature keeps room for temp-profile
// providers without touching the consumers again.
type Provider interface {
	Chrome(ctx context.Context) (path string, cleanup func(), err error)
}

// Options configures a CloakProvider. Zero values are legitimate: the
// config layer validates the vocabulary and defaults the toggles.
type Options struct {
	// AutoDownload allows step 4 of the chain (the cloakbrowser download).
	AutoDownload bool
	// Channel is "" | auto | free | pro (see the config constants).
	Channel string
	// CacheDir overrides the binary cache root; "" = CLOAKBROWSER_CACHE_DIR
	// or ~/.cloakbrowser (upstream-compatible, same precedence).
	CacheDir string
	// Version pins the download/cache version; "" = CLOAKBROWSER_VERSION
	// or the per-platform pinned free build.
	Version string
	// Preset is the [browser] launch profile (default | careful): the
	// capture session runs the same flag set as the browser lanes so
	// every session presents one identity (T82).
	Preset string
	// DownloadURL overrides the release base URL; "" = CLOAKBROWSER_DOWNLOAD_URL
	// or https://cloakbrowser.dev (GitHub Releases fallback always tried).
	DownloadURL string
}

// CloakProvider is the Provider implementation. Chrome memoizes the
// resolution (and its error) for the provider's lifetime.
type CloakProvider struct {
	opt Options
	log *slog.Logger

	once    sync.Once
	path    string
	cleanup func()
	err     error
}

// Compile-time conformance.
var _ Provider = (*CloakProvider)(nil)

// New builds a provider; a nil logger falls back to slog.Default().
func New(o Options, log *slog.Logger) *CloakProvider {
	if log == nil {
		log = slog.Default()
	}
	return &CloakProvider{opt: o, log: log}
}

// NewFromConfig builds a provider from the [browser] config section.
// AutoDownload dereferences nil as false: the toggle is activated by the
// config defaults (nil→true in applyDefaults), never implicitly — a
// zero-value config keeps the legacy no-download behavior.
func NewFromConfig(cfg config.Browser, log *slog.Logger) *CloakProvider {
	return New(Options{
		AutoDownload: cfg.AutoDownload != nil && *cfg.AutoDownload,
		Channel:      cfg.Channel,
		Preset:       cfg.Preset,
	}, log)
}

// stealthFlags maps the [browser] preset onto the capture launch flag set
// — the same profile the editor runner / testsolve filler run under, so
// the capture session and the lanes present one identity (T82). A broken
// preset degrades to the plain launch (warned), like the lanes do.
func (p *CloakProvider) stealthFlags() []StealthFlag {
	flags, err := StealthFlags(p.opt.Preset)
	if err != nil {
		p.log.Warn("browser: preset проигнорирован", "err", err)
		return nil
	}
	return flags
}

// Chrome implements Provider. The result is memoized: the first call may
// download (~200 MB), later calls return the cached path.
func (p *CloakProvider) Chrome(ctx context.Context) (string, func(), error) {
	p.once.Do(func() { p.resolve(ctx) })
	return p.path, p.cleanup, p.err
}

// resolve walks the chain once. A set-but-broken HHAUTO_CHROME_PATH is a
// hard error — the explicit setting is the single source of truth (same
// semantics as the legacy testsolve/autoclone discovery).
func (p *CloakProvider) resolve(ctx context.Context) {
	p.cleanup = func() {}

	// 1. Explicit override.
	if env := strings.TrimSpace(os.Getenv("HHAUTO_CHROME_PATH")); env != "" {
		if path, ok := lookPath(env); ok {
			p.log.Info("browser: chrome via HHAUTO_CHROME_PATH", "path", path)
			p.path = path
			return
		}
		p.err = fmt.Errorf("%w: HHAUTO_CHROME_PATH=%q не существует или не исполняем", ErrChromeMissing, env)
		return
	}

	// 2. PATH.
	if path, ok := lookPath(""); ok {
		p.log.Info("browser: chrome on PATH", "path", path)
		p.path = path
		return
	}

	// 3. Cache (~/.cloakbrowser/chromium-<ver>, compatible version only).
	root := p.cacheDir()
	if path, ok := p.cacheLookup(root); ok {
		p.log.Info("browser: chrome из кэша cloakbrowser", "path", path)
		p.path = path
		return
	}

	// 4. Auto-download. The pro channel never downloads in v1 (licensed
	// builds are cache-resolved only).
	if p.channel() == ChannelPro {
		p.err = fmt.Errorf("browser: канал %s разрешает только кэшированные pro-сборки — лицензионная загрузка вне v1 (используйте cloakbrowser CLI или канал auto/free)", ChannelPro)
		return
	}
	if !p.opt.AutoDownload {
		p.err = fmt.Errorf("%w: browser.auto_download отключён", ErrChromeMissing)
		return
	}
	path, err := p.download(ctx, root)
	if err != nil {
		p.err = err
		return
	}
	p.log.Info("browser: cloakbrowser chromium загружен и установлен", "path", path)
	p.path = path
}

// lookPath resolves one explicit name or scans the chrome-family PATH
// candidates. ok=false means nothing executable was found.
func lookPath(explicit string) (string, bool) {
	if explicit != "" {
		path, err := exec.LookPath(explicit)
		return path, err == nil
	}
	for _, name := range chromeNames {
		if path, err := exec.LookPath(name); err == nil {
			return path, true
		}
	}
	return "", false
}

// cacheDir resolves the cache root: option → CLOAKBROWSER_CACHE_DIR →
// ~/.cloakbrowser (upstream-compatible precedence).
func (p *CloakProvider) cacheDir() string {
	if p.opt.CacheDir != "" {
		return p.opt.CacheDir
	}
	if env := strings.TrimSpace(os.Getenv("CLOAKBROWSER_CACHE_DIR")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".cloakbrowser")
}

// version pins the target build: option → CLOAKBROWSER_VERSION → the
// per-platform pinned free build.
func (p *CloakProvider) version() (string, error) {
	if p.opt.Version != "" {
		return p.opt.Version, nil
	}
	if env := strings.TrimSpace(os.Getenv("CLOAKBROWSER_VERSION")); env != "" {
		return env, nil
	}
	tag, err := platformTag()
	if err != nil {
		return "", err
	}
	v, ok := pinnedFreeVersions[tag]
	if !ok {
		return "", fmt.Errorf("browser: нет закреплённой версии для платформы %s", tag)
	}
	return v, nil
}

// channel normalizes the configured channel ("" = auto).
func (p *CloakProvider) channel() string {
	if p.opt.Channel == "" {
		return ChannelAuto
	}
	return p.opt.Channel
}

// cacheLookup finds an executable cached binary for the channel. Versions
// from the update marker must be ≥ the pinned build (upstream
// _version_newer semantics); unmarked cache dirs must share the pinned
// major version (protects against junk entries).
func (p *CloakProvider) cacheLookup(root string) (string, bool) {
	tag, err := platformTag()
	if err != nil {
		return "", false
	}

	if p.channel() == ChannelPro {
		// Pro marker first, then any cached pro build (newest wins); the
		// licensed pro download is out of scope for v1.
		if v := readMarker(root, "latest_pro_version_"+tag); v != "" {
			if path := binaryIn(root, "chromium-"+v+"-pro"); path != "" {
				return path, true
			}
		}
		path := newestCached(root, "-pro", nil)
		return path, path != ""
	}

	pinned, err := p.version()
	if err != nil {
		return "", false
	}
	if p.channel() == ChannelAuto {
		if v := readMarker(root, "latest_version_"+tag); v != "" && versionGE(v, pinned) {
			if path := binaryIn(root, "chromium-"+v); path != "" {
				return path, true
			}
		}
	}
	if path := binaryIn(root, "chromium-"+pinned); path != "" {
		return path, true
	}
	major, _ := parseVersion(pinned)
	path := newestCached(root, "", func(v string) bool {
		m, ok := parseVersion(v)
		return ok && len(m) > 0 && len(major) > 0 && m[0] == major[0]
	})
	return path, path != ""
}

// readMarker reads an update marker file (upstream layout: the file body
// is the version string).
func readMarker(root, name string) string {
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// binaryIn reports the binary path inside a version dir when it exists
// and is executable.
func binaryIn(root, dirName string) string {
	rel, err := binaryRelPath()
	if err != nil {
		return ""
	}
	path := filepath.Join(root, dirName, rel)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	return path
}

// newestCached scans root for chromium-<v>[suffix] dirs, filters by keep,
// and returns the binary of the highest version ("" when none).
func newestCached(root, suffix string, keep func(v string) bool) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	bestV, bestPath := "", ""
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, "chromium-") {
			continue
		}
		rest := strings.TrimPrefix(name, "chromium-")
		if suffix == "" {
			if strings.HasSuffix(rest, "-pro") {
				continue
			}
		} else if !strings.HasSuffix(rest, suffix) {
			continue
		}
		v := strings.TrimSuffix(rest, suffix)
		if keep != nil && !keep(v) {
			continue
		}
		if path := binaryIn(root, name); path != "" && versionGE(v, bestV) {
			bestV, bestPath = v, path
		}
	}
	return bestPath
}

// parseVersion splits a dotted version into ints; ok=false on junk.
func parseVersion(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		if p == "" {
			return nil, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, false
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out, true
}

// versionGE reports a >= b in dotted-segment order; junk sorts lowest.
func versionGE(a, b string) bool {
	if b == "" {
		return true
	}
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return aok && !bok
	}
	for i := 0; i < len(av) || i < len(bv); i++ {
		x, y := 0, 0
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}
