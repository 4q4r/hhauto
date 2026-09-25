//go:build live

package hhapi

// T24 LIVE VERIFICATION (manual, network + owner session required; never in
// the default gate):
//
//	go test -tags live -run TestLiveSearchT24 -v ./internal/hhapi/
//
// Proves the tls-client transport clears the hh.ru TLS (JA3/JA3S) gate that
// the stdlib crypto/tls stack fails live (2026-09-25: 200 but a challenge
// page without HH-Lux-InitialState): GET /search/vacancy?text=python with
// owner cookies must return 200 with the SSR state node, vacancySearchResult
// inside it, and a non-empty vacancies list.
//
// Inputs (never committed, values never printed):
//   - HHAUTO_T24_CONFIG  (default /tmp/opencode/survey/hhauto.toml) — the
//     survey config supplies the account's User-Agent, endpoints and proxy.
//   - HHAUTO_T24_COOKIES (default /tmp/opencode/hh-cookies.txt) — one header
//     line: "Cookie: k=v; k2=v2; ...".

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

func liveT24Path(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

// parseCookieHeaderFile reads the single "Cookie: k=v; ..." line. The
// header carries no domain attributes, so cookies are rebuilt with the
// parent-domain scope a browser session has (.hh.ru): the regional redirect
// (hh.ru -> taganrog.hh.ru) must keep the session cookies flowing, exactly
// like the Domain=.hh.ru Set-Cookies hh itself issues.
func parseCookieHeaderFile(t *testing.T, path, siteHost string) []*http.Cookie {
	t.Helper()
	domain := "." + registrableParent(siteHost)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cookies file: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	if i := strings.Index(line, "\n"); i >= 0 {
		line = strings.TrimSpace(line[:i]) // first line only
	}
	line = strings.TrimPrefix(line, "Cookie:")
	line = strings.TrimPrefix(line, "cookie:")
	line = strings.TrimSpace(line)
	if line == "" {
		t.Fatalf("cookie file %s is empty", path)
	}
	var out []*http.Cookie
	for _, pair := range strings.Split(line, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			t.Fatalf("cookie file %s: malformed pair %q", path, pair)
		}
		out = append(out, &http.Cookie{
			Name:   strings.TrimSpace(k),
			Value:  strings.TrimSpace(v),
			Path:   "/",
			Domain: domain,
		})
	}
	return out
}

// registrableParent drops the leftmost label of a host while more than two
// labels remain ("api.hh.ru" -> "hh.ru", "hh.ru" -> "hh.ru").
func registrableParent(host string) string {
	labels := strings.Split(host, ".")
	for len(labels) > 2 {
		labels = labels[1:]
	}
	return strings.Join(labels, ".")
}

func TestLiveSearchT24(t *testing.T) {
	cfg, err := config.Load(liveT24Path("HHAUTO_T24_CONFIG", "/tmp/opencode/survey/hhauto.toml"))
	if err != nil {
		t.Fatalf("load survey config: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("expected exactly one account in the survey config, got %d", len(cfg.Accounts))
	}
	var acctID string
	var acct config.Account
	for id, a := range cfg.Accounts {
		acctID, acct = id, a
	}
	t.Logf("account %q: transport=%s ua_family=redacted(rps=%.2f)", acctID, cfg.API.Transport, acct.RPS)

	opts := []ClientOption{WithTransport(cfg.API.Transport)}
	if acct.ProxyRef != "" {
		p, ok := cfg.Proxy[acct.ProxyRef]
		if !ok {
			t.Fatalf("proxy_ref %q missing from config", acct.ProxyRef)
		}
		purl, err := proxy.URL(p)
		if err != nil {
			t.Fatalf("proxy url: %v", err)
		}
		opts = append(opts, WithProxyURL(purl))
		t.Logf("proxy_ref %q wired through tls-client SetProxy", acct.ProxyRef)
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	siteHost := ""
	if u, perr := url.Parse(cfg.API.Endpoints.Site); perr == nil {
		siteHost = u.Host
	}
	if err := c.LoadCookies(ctx, parseCookieHeaderFile(t, liveT24Path("HHAUTO_T24_COOKIES", "/tmp/opencode/hh-cookies.txt"), siteHost)); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	// Raw document request — assert the exact live-fact chain: 200, the
	// HH-Lux-InitialState node present, vacancySearchResult inside, and
	// non-empty vacancies.
	target := c.searchURL(SearchQuery{Text: "python", PerPage: 10})
	resp, err := c.getDoc(ctx, "search_vacancies", target, c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		t.Fatalf("search_vacancies request: %v", err)
	}
	if resp.status != 200 {
		t.Fatalf("status = %d, want 200 (body snippet: %.200s)", resp.status, snippet(resp.body))
	}
	t.Logf("GET %s -> 200, %d bytes, final URL: %s", strings.Split(target, "?")[0], len(resp.body), resp.finalURL)
	state, ok := initialState(resp.body)
	if !ok {
		t.Fatalf("HH-Lux-InitialState MISSING from the search page — TLS challenge still served (body snippet: %.300s)", snippet(resp.body))
	}
	vsr := obj(state["vacancySearchResult"])
	if vsr == nil {
		t.Fatalf("vacancySearchResult MISSING from state (keys: %d top-level nodes)", len(state))
	}
	items := arr(vsr["vacancies"])
	if len(items) == 0 {
		t.Fatal("vacancies list is empty")
	}
	t.Logf("state: HH-Lux-InitialState present, vacancySearchResult present, %d raw vacancy blocks", len(items))

	// Full contract path: the operation that failed live with errNilState.
	page, err := c.SearchVacancies(ctx, SearchQuery{Text: "python", PerPage: 10})
	if err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	if page.Found <= 0 || len(page.Items) == 0 {
		t.Fatalf("SearchVacancies: found=%d items=%d, want non-empty", page.Found, len(page.Items))
	}
	t.Logf("SearchVacancies OK: found=%d, first item: %q (%d)", page.Found, page.Items[0].Name, page.Items[0].ID)
}
