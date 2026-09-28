// tls_probe.go (T37 v3) captures the real cloakbrowser chromium
// ClientHello for the 1:1 TLS replay. The browser is pointed at a local
// HTTP CONNECT proxy; inside the tunnel the first bytes the client
// produces are the ClientHello in cleartext — captured verbatim while
// the connection is relayed to the real target, so TLS end-to-end
// semantics (SNI, certificate validation) stay bound to the original
// host. The proxy form is used instead of --host-resolver-rules because
// the stealth-patched cloakbrowser build does not honor resolver
// overrides, while --proxy-server is a first-class switch (the same
// mechanism the fillers already use for per-account proxies).
//
// Why not CDP: the DevTools protocol exposes only TLS version, cipher
// and ECH booleans (SecurityDetails) — never the raw hello bytes, never
// the extension list or order. A transparent tap is the only capture
// path that is byte-faithful by construction.
//
// The probe runs direct (no upstream proxy): the hello is
// proxy-independent, and the relay must dial the real target anyway.

package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

// probeTimeout bounds the whole capture (browser start + navigation +
// hello arrival). The hello is sent immediately after the CONNECT
// tunnel opens, so the budget mostly covers browser startup.
const probeTimeout = 90 * time.Second

// uaProbeTimeout bounds the navigator.userAgent read that opens the
// browser session; browser startup is the only slow part.
const uaProbeTimeout = 45 * time.Second

// ProbeHost extracts the hostname of the configured site URL — the
// default capture host for the TLS spec.
func ProbeHost(site string) string {
	u, err := url.Parse(site)
	if err != nil || u.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(site, "https://"), "http://")
	}
	return u.Hostname()
}

// CaptureTLSSpec is the shared capture used by both the tls-probe command
// and the engine's automatic capture (T50): it probes the cloakbrowser
// configured in [browser] (auto_download included) for its ClientHello
// and persists the spec atomically at the default cache path. An empty
// host means the hostname of the configured site URL. The returned spec
// carries the browser's own User-Agent.
func CaptureTLSSpec(ctx context.Context, cfg *config.Config, host string, log *slog.Logger) (*tlsfp.Spec, error) {
	if cfg == nil {
		return nil, errors.New("browser: tls capture: nil config")
	}
	if host == "" {
		host = ProbeHost(cfg.API.Endpoints.Site)
	}
	provider := NewFromConfig(cfg.Browser, log)
	spec, err := ProbeTLSSpec(ctx, provider, host, log)
	if err != nil {
		return nil, err
	}
	path := tlsfp.DefaultPath(cfg.General.DataDir)
	if err := spec.Save(path); err != nil {
		return nil, fmt.Errorf("browser: tls capture: save: %w", err)
	}
	return spec, nil
}

// ProbeTLSSpec launches the cloakbrowser chromium through a local
// CONNECT tap, captures its ClientHello against host and returns the
// parsed, replay-verified spec. The browser's navigator.userAgent is
// read in the same session and stamped into the spec (T50: the capture
// is the single source of truth for the UA too). The browser is torn
// down as soon as the hello is captured; the target page never needs to
// finish loading.
func ProbeTLSSpec(ctx context.Context, p *CloakProvider, host string, log *slog.Logger) (*tlsfp.Spec, error) {
	if host == "" {
		return nil, errors.New("browser: tls probe: empty host")
	}
	chromePath, cleanup, err := p.Chrome(ctx)
	if err != nil {
		return nil, fmt.Errorf("browser: tls probe: resolve chrome: %w", err)
	}
	defer cleanup()
	ver := ChromeVersion(ctx, chromePath, log)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("browser: tls probe: listen: %w", err)
	}
	defer func() { _ = ln.Close() }()
	captured := make(chan []byte, 4)
	go serveTap(ctx, ln, allowTLS443, captured)

	// Throwaway profile: a fresh browser state guarantees a
	// first-connection hello (no session resumption, no PSK identities —
	// replaying those would be a replay signal).
	dir, err := os.MkdirTemp("", "hhauto-tlsprobe-")
	if err != nil {
		return nil, fmt.Errorf("browser: tls probe: temp profile: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(dir),
		chromedp.ProxyServer(ln.Addr().String()),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	navCtx, cancelNav := context.WithTimeout(browserCtx, probeTimeout)
	defer func() {
		cancelNav()
		cancelBrowser()
		cancelAlloc()
	}()

	// The UA is read from the browser's own about:blank session before
	// any navigation — deterministic, and the same value the browser
	// puts into its User-Agent request header. A capture without a UA
	// is incomplete (the spec is the UA source of truth) and fails.
	uaCtx, cancelUA := context.WithTimeout(browserCtx, uaProbeTimeout)
	defer cancelUA()
	var ua string
	if uerr := chromedp.Run(uaCtx, chromedp.Evaluate(`navigator.userAgent`, &ua)); uerr != nil {
		return nil, fmt.Errorf("browser: tls probe: read browser User-Agent: %w", uerr)
	}
	if strings.TrimSpace(ua) == "" {
		return nil, errors.New("browser: tls probe: browser reported an empty User-Agent")
	}

	go func() {
		// Navigation errors are irrelevant to the capture — the hello
		// arrives as soon as the tunnel opens, long before any
		// page-level outcome — but they are the probe's only window
		// into browser failures.
		if nerr := chromedp.Run(navCtx, chromedp.Navigate("https://"+host+"/")); nerr != nil {
			log.Debug("browser: tls probe: navigation finished with an error", "error", nerr)
		}
	}()

	select {
	case raw := <-captured:
		meta := tlsfp.CaptureMeta{
			CapturedAt:  time.Now().UTC().Format(time.RFC3339),
			BrowserPath: chromePath,
			BrowserVer:  ver,
			Host:        host,
			UserAgent:   ua,
		}
		spec, err := tlsfp.ParseClientHello(raw, meta)
		if err != nil {
			return nil, fmt.Errorf("browser: tls probe: parse captured hello: %w", err)
		}
		if err := spec.Verify(2); err != nil {
			return nil, fmt.Errorf("browser: tls probe: %w", err)
		}
		log.Info("browser: captured the cloakbrowser ClientHello",
			"host", host, "browser_version", ver, "user_agent", ua,
			"extensions", len(spec.Extensions), "ja3_md5", spec.JA3MD5)
		return spec, nil
	case <-time.After(probeTimeout):
		return nil, fmt.Errorf("browser: tls probe: no ClientHello within %s", probeTimeout)
	case <-ctx.Done():
		return nil, fmt.Errorf("browser: tls probe: %w", ctx.Err())
	}
}

// serveTap accepts browser connections on the local CONNECT proxy. The
// allow predicate guards which tunnel targets the tap is willing to
// open (production: TLS ports only).
func serveTap(ctx context.Context, ln net.Listener, allow func(host, port string) bool, captured chan<- []byte) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close() //nolint:errcheck // tap relay
			relayAndSniff(ctx, c, allow, captured)
		}(conn)
	}
}

// allowTLS443 is the production target guard: TLS on 443 only, so the
// tap can never serve as an open relay.
func allowTLS443(_, port string) bool { return port == "443" }

// relayAndSniff serves one CONNECT tunnel: opens the tunnel to the real
// target, posts the tunnel's first client bytes (the ClientHello) for
// capture, then splices both directions so the browser keeps talking to
// the real server normally.
func relayAndSniff(ctx context.Context, c net.Conn, allow func(host, port string) bool, captured chan<- []byte) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	target, err := connectTarget(c, allow)
	if err != nil {
		return
	}
	up, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		_ = writeSimple(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close() //nolint:errcheck // tap relay
	if err := writeSimple(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}

	// First client bytes inside the tunnel are the TLS ClientHello;
	// anything else is relayed untouched — the tunnel must stay
	// byte-exact no matter what the browser sends.
	raw, isHello, serr := tlsfp.SniffClientHelloRecord(c)
	if serr != nil {
		return
	}
	if isHello {
		select {
		case captured <- raw:
		default: // a later connection's hello is not needed
		}
	}
	if _, werr := up.Write(raw); werr != nil {
		return
	}
	_ = c.SetDeadline(time.Time{}) // clear for the relay

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, c)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, up)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(probeTimeout):
	case <-ctx.Done():
	}
}

// connectTarget reads the CONNECT request and returns its host:port
// when the allow predicate accepts it — the tap exists to capture
// handshakes, and it must never become an open relay.
func connectTarget(c net.Conn, allow func(host, port string) bool) (string, error) {
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return "", err
	}
	if req.Method != http.MethodConnect {
		return "", fmt.Errorf("browser: tls probe: unexpected %s", req.Method)
	}
	host := req.Host
	if host == "" {
		return "", errors.New("browser: tls probe: empty CONNECT target")
	}
	hostname, port, err := net.SplitHostPort(host)
	if err != nil || !allow(hostname, port) {
		return "", fmt.Errorf("browser: tls probe: CONNECT target not allowed: %q", host)
	}
	return host, nil
}

func writeSimple(c net.Conn, resp string) error {
	_, err := io.WriteString(c, resp)
	return err
}

// ChromeVersion resolves the browser version string (`chrome --version`
// is a cheap exec, no CDP needed). Failures degrade to the executable
// name: the version is provenance metadata for the spec stamp, not
// replay input.
func ChromeVersion(ctx context.Context, chromePath string, log *slog.Logger) string {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, chromePath, "--version").Output()
	if err != nil {
		log.Warn("browser: tls probe: chrome --version failed; stamping the spec with the executable name only", "error", err)
		base := chromePath[strings.LastIndexByte(chromePath, '/')+1:]
		return base
	}
	return strings.TrimSpace(string(out))
}
