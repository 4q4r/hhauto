// download.go implements step 4 of the provider chain: the cloakbrowser
// chromium auto-download. The mechanics mirror the upstream wrapper
// (CloakHQ/CloakBrowser cloakbrowser/config.py):
//
//	archive   {base}/chromium-v<ver>/cloakbrowser-<tag>.tar.gz|.zip
//	checksums {base}/chromium-v<ver>/SHA256SUMS   (verified when published)
//	base      CLOAKBROWSER_DOWNLOAD_URL or https://cloakbrowser.dev,
//	          fallback https://github.com/CloakHQ/cloakbrowser/releases/download
//	marker    latest_version_<tag> written after a successful install
//
// Timeout/retry semantics: one attempt per base URL (dev first, GitHub
// fallback second), each bounded by a 10-minute context deadline on top of
// the caller's ctx. Partial installs never reach the version directory.

package browser

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// downloadTimeout bounds one archive/checksum fetch.
const downloadTimeout = 10 * time.Minute

// downloadProgressStep is the byte distance between progress log lines.
const downloadProgressStep = 32 << 20

// pinnedFreeVersions maps a platform tag to the free GitHub build pinned
// for it (upstream keeps the free 146 line on GitHub Releases; macOS stays
// on the 150 line — release notes of CloakBrowser v0.5.x).
var pinnedFreeVersions = map[string]string{
	"linux-x64":   "146.0.7680.177.5",
	"linux-arm64": "146.0.7680.177.5",
	"windows-x64": "146.0.7680.177.5",
	"macos-x64":   "150.0.7871.114.3",
	"macos-arm64": "150.0.7871.114.3",
}

// releaseBaseURLs are tried in order: the wrapper's dev endpoint, then the
// public GitHub Releases (the free build's canonical home).
var releaseBaseURLs = []string{
	"https://cloakbrowser.dev",
	"https://github.com/CloakHQ/cloakbrowser/releases/download",
}

// platformTag mirrors the upstream archive naming (linux-x64,
// linux-arm64, windows-x64 are release-verified; the darwin tags follow
// the same scheme but are best-effort).
func platformTag() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux-x64", nil
	case "linux/arm64":
		return "linux-arm64", nil
	case "windows/amd64":
		return "windows-x64", nil
	case "darwin/amd64":
		return "macos-x64", nil
	case "darwin/arm64":
		return "macos-arm64", nil
	default:
		return "", fmt.Errorf("browser: платформа %s/%s не поддерживается загрузчиком cloakbrowser", runtime.GOOS, runtime.GOARCH)
	}
}

// binaryRelPath is the executable's path inside an extracted version dir.
func binaryRelPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join("Chromium.app", "Contents", "MacOS", "Chromium"), nil
	case "windows":
		return "chrome.exe", nil
	default:
		return "chrome", nil
	}
}

// archiveName is the release asset for a platform tag (.zip on Windows,
// .tar.gz elsewhere).
func archiveName(tag string) string {
	if runtime.GOOS == "windows" {
		return "cloakbrowser-" + tag + ".zip"
	}
	return "cloakbrowser-" + tag + ".tar.gz"
}

// downloadBaseURLs resolves the tried base URLs (option override wins,
// then the upstream env, then the built-ins).
func (p *CloakProvider) downloadBaseURLs() []string {
	if p.opt.DownloadURL != "" {
		return []string{strings.TrimRight(p.opt.DownloadURL, "/")}
	}
	if env := strings.TrimSpace(os.Getenv("CLOAKBROWSER_DOWNLOAD_URL")); env != "" {
		return []string{strings.TrimRight(env, "/")}
	}
	return releaseBaseURLs
}

// download installs the pinned build into the cache root and returns the
// binary path.
func (p *CloakProvider) download(ctx context.Context, root string) (string, error) {
	version, err := p.version()
	if err != nil {
		return "", err
	}
	tag, err := platformTag()
	if err != nil {
		return "", err
	}
	name := archiveName(tag)

	// Staging dir: everything happens here; the version dir appears only
	// after a verified extraction.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("browser: cache dir: %w", err)
	}
	work, err := os.MkdirTemp(root, ".download-")
	if err != nil {
		return "", fmt.Errorf("browser: staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	archivePath := filepath.Join(work, name)
	if err := p.fetchArchive(ctx, version, name, archivePath); err != nil {
		return "", err
	}

	out := filepath.Join(work, "out")
	if runtime.GOOS == "windows" {
		err = extractZip(archivePath, out)
	} else {
		err = extractTarGz(archivePath, out)
	}
	if err != nil {
		return "", fmt.Errorf("browser: extract %s: %w", name, err)
	}
	rel, err := binaryRelPath()
	if err != nil {
		return "", err
	}
	binary := filepath.Join(out, rel)
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		return "", fmt.Errorf("browser: в архиве %s нет исполняемого %s", name, rel)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return "", fmt.Errorf("browser: chmod binary: %w", err)
	}

	// Publish atomically-ish: replace any stale dir, then rename.
	final := filepath.Join(root, "chromium-"+version)
	if err := os.RemoveAll(final); err != nil {
		return "", fmt.Errorf("browser: replace stale version dir: %w", err)
	}
	if err := os.Rename(out, final); err != nil {
		return "", fmt.Errorf("browser: install: %w", err)
	}

	// Auto-update marker for the next run (upstream layout).
	if err := os.WriteFile(filepath.Join(root, "latest_version_"+tag), []byte(version), 0o644); err != nil {
		p.log.Warn("browser: marker write failed (cache still usable)", "err", err)
	}
	return filepath.Join(final, rel), nil
}

// fetchArchive downloads the archive with checksum verification when the
// source publishes SHA256SUMS. One attempt per base URL.
func (p *CloakProvider) fetchArchive(ctx context.Context, version, name, dst string) error {
	var lastErr error
	for _, base := range p.downloadBaseURLs() {
		url := base + "/chromium-v" + version + "/" + name
		sum, haveSum, sumErr := fetchChecksum(ctx, base+"/chromium-v"+version+"/SHA256SUMS", name)
		if sumErr != nil {
			p.log.Warn("browser: SHA256SUMS недоступен — целостность не проверяется",
				"url", base, "err", sumErr)
		}
		if err := downloadFile(ctx, p.log, url, dst); err != nil {
			lastErr = err
			continue
		}
		if haveSum {
			got, err := fileSHA256(dst)
			if err != nil {
				lastErr = err
				continue
			}
			if got != sum {
				_ = os.Remove(dst)
				lastErr = fmt.Errorf("browser: SHA-256 checksum mismatch для %s: получено %s, ожидалось %s", name, got, sum)
				continue
			}
			p.log.Info("browser: SHA-256 сверён", "archive", name)
		}
		return nil
	}
	return lastErr
}

// fetchChecksum parses SHA256SUMS for the archive entry ("sum␠␠name").
func fetchChecksum(ctx context.Context, url, name string) (string, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("SHA256SUMS: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[1] == name {
			return fields[0], true, nil
		}
	}
	return "", false, errors.New("SHA256SUMS: записи нет")
}

// downloadFile streams the archive to dst with progress logging.
func downloadFile(ctx context.Context, log *slog.Logger, url, dst string) error {
	cctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	log.Info("browser: загрузка cloakbrowser chromium", "url", url, "size_hint_mb", resp.ContentLength/(1<<20))
	var downloaded int64
	next := int64(downloadProgressStep)
	buf := make([]byte, 512<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			downloaded += int64(n)
			if downloaded >= next {
				log.Info("browser: chromium download progress", "mb", downloaded/(1<<20))
				next += downloadProgressStep
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("download %s: %w", url, rerr)
		}
	}
	return f.Sync()
}

// httpClient returns the shared download client (pure Go, CGO-free).
func httpClient() *http.Client {
	// A fresh client per call is fine here: downloads are rare (once per
	// version) and the transport needs no shared state.
	return &http.Client{}
}

// fileSHA256 hex-digests a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safeJoin refuses paths escaping dst (zip-slip / tar-bomb guard).
func safeJoin(dst, name string) (string, error) {
	p := filepath.Join(dst, filepath.FromSlash(name))
	if p != dst && !strings.HasPrefix(p, dst+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the extraction dir", name)
	}
	return p, nil
}

// extractTarGz unpacks a .tar.gz archive (regular files, dirs, symlinks
// inside the target only).
func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if err := writeFile(tr, target, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil && !os.IsExist(err) {
				return err
			}
		default:
			return fmt.Errorf("неподдерживаемый тип записи %q (%v)", hdr.Name, hdr.Typeflag)
		}
	}
}

// extractZip unpacks a .zip archive (Windows builds).
func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	for _, zf := range zr.File {
		target, err := safeJoin(dst, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeFile(rc, target, zf.Mode()&0o777)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFile copies r to path with the given mode (no over-following
// symlinks: any existing entry is replaced first).
func writeFile(r io.Reader, path string, mode os.FileMode) error {
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
