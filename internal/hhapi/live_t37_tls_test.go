//go:build live

package hhapi

// T37 v3 LIVE VERIFICATION (manual, network + a captured browser spec
// required; never in the default gate):
//
//	go test -tags live -run TestLiveBrowserTLSReplayT37 -v ./internal/hhapi/
//
// Proves the captured-hello replay clears the hh.ru TLS fingerprint gate
// end to end: an unauthenticated GET of the site root through the
// tls-client transport built with WithBrowserTLSSpec must return 200 with
// the normal homepage — not a 403/challenge page (the fingerprint-rejected
// outcome the stdlib stack gets, see live_t24_search_test.go).
//
// Inputs (never committed, values never printed):
//   - HHAUTO_T37_CONFIG (default: the standard config path) — supplies the
//     data_dir with the spec cache and the site endpoint.
//   - HHAUTO_T37_SPEC   (default: <data_dir>/browser/tls_spec.json) — the
//     capture produced by `hhauto tls-probe`.

import (
	"context"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

func TestLiveBrowserTLSReplayT37(t *testing.T) {
	cfgPath := os.Getenv("HHAUTO_T37_CONFIG")
	if cfgPath == "" {
		cfgPath = config.ExpandPath("~/.config/hhauto/hhauto.toml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Skipf("config not reachable: %v", err)
	}
	specPath := os.Getenv("HHAUTO_T37_SPEC")
	if specPath == "" {
		specPath = tlsfp.DefaultPath(cfg.General.DataDir)
	}
	if specPath == filepath.Base(specPath) {
		t.Skipf("spec path missing")
	}
	spec, err := tlsfp.Load(specPath)
	if err != nil {
		t.Skipf("captured spec not reachable: %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	tp, err := newTLSTransport(jar, "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/"+spec.BrowserVer+" Safari/537.36", "", spec, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}

	site := cfg.API.Endpoints.Site
	if site == "" {
		site = "https://hh.ru"
	}
	u, err := url.Parse(site + "/")
	if err != nil {
		t.Fatalf("site url: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Manual redirect hops (the transport is single-hop by contract): a
	// regional 302 is a PASS signal — the hello was accepted and routed.
	var resp *hopResponse
	hops := 0
	for {
		resp, err = tp.roundTrip(ctx, "GET", u, nil, nil)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		t.Logf("hop %d: %s -> %d", hops, u, resp.status)
		if loc := resp.header.Get("Location"); resp.status >= 300 && resp.status < 400 && loc != "" {
			hops++
			if hops > 5 {
				t.Fatalf("too many redirects")
			}
			next, perr := u.Parse(loc)
			if perr != nil {
				t.Fatalf("bad Location %q: %v", loc, perr)
			}
			u = next
			continue
		}
		break
	}
	body := strings.ToLower(string(resp.body))
	t.Logf("final: status=%d bytes=%d ja3_md5=%s", resp.status, len(resp.body), spec.JA3MD5)
	if resp.status != 200 {
		t.Fatalf("expected 200 after redirects, got %d (fingerprint or gate rejected)", resp.status)
	}
	// Challenge pages are tiny; the real homepage is megabytes. The word
	// "captcha" alone appears in normal hh scripts, so the gate check is
	// size plus the ddg challenge cookie-script marker.
	if strings.Contains(body, "__ddg_=") || len(resp.body) < 50_000 {
		t.Fatalf("the response looks like a challenge page (%d bytes), not the homepage", len(resp.body))
	}
	t.Log("LIVE OK: the captured-hello replay cleared the hh.ru TLS gate")
}
