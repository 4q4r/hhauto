package hhapi

// T24 transport: hh.ru fingerprintes the TLS handshake (JA3/JA3S). The
// stdlib crypto/tls stack gets 200-but-challenge pages without the
// HH-Lux-InitialState node on document surfaces while JSON shards pass
// (live-verified 2026-09-25), so document requests must ride a
// browser-consistent TLS fingerprint. This file adapts bogdanfinn/tls-client
// (pure-Go uTLS, BSD-4-Clause) onto the existing request pipeline without
// touching the HHAPI contract.
//
// Control mapping (what the legacy http.Client path provided and where it
// lives now):
//   - Cookie jar + CookiePersister: the shared net/http cookiejar stays the
//     single cookie authority. tls-client gets NO jar of its own; cookies are
//     emitted from the shared jar before every hop and Set-Cookie is absorbed
//     into the jar + persistence book after every hop, so redirect-hop
//     cookies (challenge pages set __ddg_* right on the 302) reach the
//     persister exactly as before.
//   - Redirect policy: tls-client is built with WithNotFollowRedirects and
//     Client.attemptTLS follows hops itself with the same semantics the
//     legacy path gets from CheckRedirect: 10-hop budget, same-registrable-
//     domain fence, regional host pinning, chain recording,
//     301/302/303 -> GET (HEAD stays HEAD), 307/308 preserve method+body.
//   - Retry/backoff, rate limiting, typed errors: unchanged — they wrap
//     Client.attempt, which is where the transport branch lives.
//   - Proxy: tls-client SetProxy via the WithProxyUrl constructor option;
//     socks5/socks5h/http/https URLs with credentials in the userinfo
//     (see proxy.URL).
//   - Timeouts: WithTimeoutSeconds(30) mirrors the legacy http.Client
//     timeout of 30s, applied per physical hop (the legacy budget covers a
//     whole redirect chain) — documented deviation.
//
// Header order: fhttp writes headers in map order unless the magic
// Header-Order: key is present, so every request carries a deterministic,
// browser-plausible order (see headerOrder). Unknown extra headers are
// appended in sorted order. fhttp's http2 writer lowercases and adds
// accept-encoding automatically.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"hhauto/internal/tlsfp"
)

// tlsClientTimeout mirrors the legacy http.Client.Timeout.
const tlsClientTimeout = 30

// tlsProfile is the selected browser profile for a User-Agent. T50: it
// feeds only the HTTP/2 settings base of the captured-hello transport —
// the TLS layer itself is always the captured spec.
type tlsProfile struct {
	name    string // e.g. "chrome_133"
	profile profiles.ClientProfile
}

type tlsProfileEntry struct {
	major   int
	name    string
	profile profiles.ClientProfile
}

// tlsChromeProfileTable and tlsFirefoxProfileTable list every browser
// profile tls-client v1.16.0 ships, ascending. profileForUA picks the
// greatest entry with major <= the UA major, so a UA never gets a TLS stack
// NEWER than the version it advertises.
var tlsChromeProfileTable = []tlsProfileEntry{
	{103, "chrome_103", profiles.Chrome_103},
	{104, "chrome_104", profiles.Chrome_104},
	{105, "chrome_105", profiles.Chrome_105},
	{106, "chrome_106", profiles.Chrome_106},
	{107, "chrome_107", profiles.Chrome_107},
	{108, "chrome_108", profiles.Chrome_108},
	{109, "chrome_109", profiles.Chrome_109},
	{110, "chrome_110", profiles.Chrome_110},
	{111, "chrome_111", profiles.Chrome_111},
	{112, "chrome_112", profiles.Chrome_112},
	{117, "chrome_117", profiles.Chrome_117},
	{120, "chrome_120", profiles.Chrome_120},
	{124, "chrome_124", profiles.Chrome_124},
	{131, "chrome_131", profiles.Chrome_131},
	{133, "chrome_133", profiles.Chrome_133},
	{144, "chrome_144", profiles.Chrome_144},
	{146, "chrome_146", profiles.Chrome_146},
	{150, "chrome_150", profiles.Chrome_150},
	{152, "chrome_152", profiles.Chrome_152},
}

var tlsFirefoxProfileTable = []tlsProfileEntry{
	{102, "firefox_102", profiles.Firefox_102},
	{104, "firefox_104", profiles.Firefox_104},
	{105, "firefox_105", profiles.Firefox_105},
	{106, "firefox_106", profiles.Firefox_106},
	{108, "firefox_108", profiles.Firefox_108},
	{110, "firefox_110", profiles.Firefox_110},
	{117, "firefox_117", profiles.Firefox_117},
	{120, "firefox_120", profiles.Firefox_120},
	{123, "firefox_123", profiles.Firefox_123},
	{132, "firefox_132", profiles.Firefox_132},
	{133, "firefox_133", profiles.Firefox_133},
	{135, "firefox_135", profiles.Firefox_135},
	{147, "firefox_147", profiles.Firefox_147},
	{148, "firefox_148", profiles.Firefox_148},
}

var firefoxMajorRe = regexp.MustCompile(`Firefox/(\d+)`)

// profileForUA maps a User-Agent onto the closest available tls-client
// browser profile: same family (Chrome via the "Chrome/" token, Edge
// included; Firefox via "Firefox/"), greatest profile major <= the UA major.
// UAs from unknown families fall back to the library default
// (profiles.DefaultClientProfile). T50: this profile contributes only the
// HTTP/2 settings/pseudo-header base of the captured-hello transport —
// the TLS fingerprint itself always comes from the captured spec.
func profileForUA(ua string) tlsProfile {
	if m := chromeMajorRe.FindStringSubmatch(ua); m != nil {
		return pickTLSProfile(tlsChromeProfileTable, m[1])
	}
	if m := firefoxMajorRe.FindStringSubmatch(ua); m != nil {
		return pickTLSProfile(tlsFirefoxProfileTable, m[1])
	}
	return tlsProfile{name: "chrome_150", profile: profiles.DefaultClientProfile}
}

func pickTLSProfile(table []tlsProfileEntry, major string) tlsProfile {
	v, err := strconv.Atoi(major)
	if err != nil || len(table) == 0 {
		return tlsProfile{name: "chrome_150", profile: profiles.DefaultClientProfile}
	}
	best := table[0]
	for _, e := range table {
		if e.major > v {
			break
		}
		best = e
	}
	return tlsProfile{name: best.name, profile: best.profile}
}

// headerOrder is the deterministic wire order for the headers this client
// ever sets (browser-plausible; family-specific differences are second-order
// next to the TLS fingerprint itself). Keys not listed here are appended in
// sorted order.
var headerOrder = []string{
	"user-agent",
	"accept",
	"accept-language",
	"upgrade-insecure-requests",
	"sec-fetch-dest",
	"sec-fetch-mode",
	"sec-fetch-user",
	"sec-fetch-site",
	"referer",
	"origin",
	"x-requested-with",
	"x-xsrftoken",
	"content-type",
	"cookie",
}

// --- Client options (contract-preserving extension of NewClient) ---

// clientOptions collects transport-level selections set via ClientOption.
type clientOptions struct {
	transport   string      // "" => config.TransportTLSClient
	proxyURL    string      // tls-client only: full proxy URL (SetProxy form)
	browserSpec *tlsfp.Spec // tls-client only: captured browser hello (1:1 replay)
}

// ClientOption extends NewClient without changing the HHAPI contract or the
// existing call sites (variadic).
type ClientOption func(*clientOptions)

// WithTransport selects the HTTP stack: config.TransportTLSClient (default;
// browser TLS fingerprint) or config.TransportHTTP (legacy stdlib transport,
// debugging fallback).
func WithTransport(mode string) ClientOption {
	return func(o *clientOptions) { o.transport = mode }
}

// WithProxyURL routes the tls-client transport through the given proxy URL
// (socks5://, socks5h://, http://, https:// with credentials in the
// userinfo). It is the constructor form of tls-client's SetProxy and is
// rejected for the legacy http transport (which proxies via its Dialer).
func WithProxyURL(raw string) ClientOption {
	return func(o *clientOptions) { o.proxyURL = raw }
}

// WithBrowserTLSSpec switches the tls-client transport onto a 1:1 replay
// of the captured cloakbrowser ClientHello (T37 v3): the TLS layer sends
// exactly what the browser sends — key shares, GREASE, ECH grease and
// session ids are still regenerated per connection by uTLS, extension
// order is permuted per connection like Chrome ≥110 does, and only the
// HTTP/2 settings keep coming from the reference profile (TLS bytes are
// the owner-visible fingerprint; H2 SETTINGS cannot be captured without
// breaking certificate validation). T50 strict parity: the spec is
// mandatory — nil or a spec that fails to build is a construction error,
// never a preset fallback.
func WithBrowserTLSSpec(spec *tlsfp.Spec) ClientOption {
	return func(o *clientOptions) { o.browserSpec = spec }
}

// newTLSTransport builds the tls-client-backed transport for one account.
// The shared net/http cookiejar stays the cookie authority (see the file
// header); the TLS layer is the captured browser hello (WithBrowserTLSSpec,
// mandatory since T50) and the HTTP/2 layer comes from the reference
// profileForUA profile for the User-Agent.
func newTLSTransport(jar *cookiejar.Jar, ua, proxyURL string, browserSpec *tlsfp.Spec, log *slog.Logger) (*tlsTransport, error) {
	if browserSpec == nil {
		return nil, errors.New("hhapi: tls-client transport requires a captured browser TLS spec (WithBrowserTLSSpec): the engine captures it automatically; hhauto tls-probe reruns the capture")
	}
	prof, err := browserClientProfile(ua, browserSpec)
	if err != nil {
		return nil, fmt.Errorf("hhapi: captured browser TLS spec unusable: %w", err)
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(tlsClientTimeout),
		tls_client.WithNotFollowRedirects(),
		// Chrome ≥110 permutes extension order on every connection;
		// uTLS reproduces that distribution (GREASE/padding/PSK stay
		// positionally fixed). A frozen captured order would itself
		// be a replay signal.
		tls_client.WithClientProfile(prof),
		tls_client.WithRandomTLSExtensionOrder(),
	}
	log.Info("hhapi: tls-client replays the captured browser ClientHello (1:1)",
		"browser_version", browserSpec.BrowserVer,
		"captured_host", browserSpec.Host,
		"ja3_md5", browserSpec.JA3MD5)
	tc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("hhapi: tls-client: %w", err)
	}
	if proxyURL != "" {
		if err := tc.SetProxy(proxyURL); err != nil {
			return nil, fmt.Errorf("hhapi: tls-client proxy: %w", err)
		}
	}
	return &tlsTransport{tc: tc, jar: jar}, nil
}

// browserClientProfile builds the tls-client profile whose TLS layer is
// the captured browser hello and whose HTTP/2 layer comes from the
// reference preset profileForUA selects for the same User-Agent (closest
// shipped settings/pseudo-header order; see WithBrowserTLSSpec).
func browserClientProfile(ua string, spec *tlsfp.Spec) (profiles.ClientProfile, error) {
	if spec == nil {
		return profiles.ClientProfile{}, fmt.Errorf("hhapi: nil browser TLS spec")
	}
	id := spec.ClientHelloID()
	if _, err := id.SpecFactory(); err != nil {
		return profiles.ClientProfile{}, err
	}
	base := profileForUA(ua).profile
	return profiles.NewClientProfile(
		id,
		base.GetSettings(),
		base.GetSettingsOrder(),
		base.GetPseudoHeaderOrder(),
		base.GetConnectionFlow(),
		base.GetPriorities(),
		base.GetHeaderPriority(),
		base.GetStreamID(),
		base.GetAllowHTTP(),
		base.GetHttp3Settings(),
		base.GetHttp3SettingsOrder(),
		base.GetHttp3PriorityParam(),
		base.GetHttp3PseudoHeaderOrder(),
		base.GetHttp3SendGreaseFrames(),
	), nil
}

// tlsTransport adapts net/http requests onto the tls-client HttpClient.
// Single-hop only: redirects are followed by Client.attemptTLS so the
// fence/pinning/hop-cookie policy stays in one place.
type tlsTransport struct {
	tc  tls_client.HttpClient
	jar *cookiejar.Jar
}

// hopResponse is one physical response with its parsed Set-Cookie list.
type hopResponse struct {
	status  int
	header  http.Header
	body    []byte
	cookies []*http.Cookie
}

// roundTrip sends one request through tls-client and reads the response.
// net/http types are converted to/from the fhttp fork at this boundary;
// nothing else in the pipeline sees fhttp types.
func (t *tlsTransport) roundTrip(ctx context.Context, method string, u *url.URL, header http.Header, body []byte) (*hopResponse, error) {
	freq, err := fhttp.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hhapi: build tls-client request: %w", err)
	}
	// Fresh map: a direct fhttp.Header(header) conversion would alias the
	// caller's map, and adding the order key below must not leak into it.
	freq.Header = fhttp.Header{}
	for k, vv := range header {
		for _, v := range vv {
			freq.Header.Add(k, v)
		}
	}
	// Cookie emission from the shared jar — the job http.Client.Jar does on
	// the legacy path. Explicitly configured Cookie headers are preserved.
	if cks := t.jar.Cookies(u); len(cks) > 0 {
		vals := freq.Header.Values("Cookie")
		for _, ck := range cks {
			vals = append(vals, ck.Name+"="+ck.Value)
		}
		freq.Header.Del("Cookie")
		for _, v := range vals {
			freq.Header.Add("Cookie", v)
		}
	}
	freq.Header[fhttp.HeaderOrderKey] = orderedHeaderKeys(freq.Header)
	if len(body) > 0 {
		freq.ContentLength = int64(len(body))
	}
	fresp, err := t.tc.Do(freq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fresp.Body.Close() }() // body fully consumed below
	b, err := io.ReadAll(io.LimitReader(fresp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("hhapi: read tls-client body: %w", err)
	}
	out := &hopResponse{
		status: fresp.StatusCode,
		header: http.Header(fresp.Header),
		body:   b,
	}
	for _, line := range fresp.Header.Values("Set-Cookie") {
		if ck, perr := http.ParseSetCookie(line); perr == nil && ck != nil {
			out.cookies = append(out.cookies, ck)
		}
	}
	return out, nil
}

// orderedHeaderKeys renders the Header-Order: value: known headers in
// browser-plausible order, everything else appended sorted. fhttp matches
// order entries against lowercased header names and never drops headers
// missing from the list (they sort last).
func orderedHeaderKeys(h fhttp.Header) []string {
	present := make(map[string]bool, len(h))
	for k := range h {
		present[strings.ToLower(k)] = true
	}
	out := make([]string, 0, len(h))
	for _, k := range headerOrder {
		if present[k] {
			out = append(out, k)
			present[k] = false
		}
	}
	var rest []string
	for k, ok := range present {
		if ok {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// isRedirectStatus reports whether the code makes net/http follow a hop.
func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// attemptTLS performs one logical round trip over the tls-client transport:
// manual redirect following with the exact policy the legacy path gets from
// CheckRedirect — 10-hop budget, same-registrable-domain fence, regional
// host pinning, hop-cookie absorption into jar and persistence book, and
// chain recording (301/302/303 -> GET, HEAD stays HEAD; 307/308 keep the
// method and body).
func (c *Client) attemptTLS(ctx context.Context, spec requestSpec) (*response, error) {
	chainCtx, chain := withRedirectChain(ctx)
	u, err := url.Parse(spec.url)
	if err != nil {
		return nil, fmt.Errorf("hhapi: parse url: %w", err)
	}
	method := spec.method
	body := spec.body
	for hop := 0; ; hop++ {
		if hop >= maxRedirects {
			return nil, errors.New("hhapi: stopped after 10 redirects")
		}
		h, err := c.tls.roundTrip(chainCtx, method, u, spec.header, body)
		if err != nil {
			return nil, err
		}
		loc := h.header.Get("Location")
		if isRedirectStatus(h.status) && loc != "" {
			next, ferr := c.tlsFollow(u, h.status, loc, h.cookies, chain)
			if ferr != nil {
				return nil, ferr
			}
			// 301/302/303 downgrade to GET (HEAD stays HEAD); 307/308
			// keep method and body — net/http semantics.
			if h.status != http.StatusTemporaryRedirect && h.status != http.StatusPermanentRedirect {
				if method != http.MethodHead {
					method = http.MethodGet
				}
				body = nil
			}
			u = next
			continue
		}
		// Final response. Its Set-Cookie must land in the shared jar too —
		// the legacy path gets this from http.Client.Jar internals; the
		// persistence book is fed by do()'s absorbAndPersist.
		c.jar.SetCookies(u, h.cookies)
		return &response{
			status:     h.status,
			header:     h.header,
			body:       h.body,
			finalURL:   u.String(),
			chain:      chain.snapshot(),
			setCookies: h.cookies,
			retryAfter: retryAfterFrom(h.header),
		}, nil
	}
}

// tlsFollow validates one redirect hop and returns its target without
// issuing anything: parse the Location (fatal on garbage, matching
// net/http), record the chain hop, absorb hop cookies into jar and book
// (challenge pages set __ddg_* right on the 302, before any final response
// exists), fence offsite hops and pin the regional host.
func (c *Client) tlsFollow(u *url.URL, status int, loc string, cookies []*http.Cookie, chain *redirectChain) (*url.URL, error) {
	next, err := u.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("hhapi: failed to parse Location header %q: %w", loc, err)
	}
	chain.add(status, loc)
	c.jar.SetCookies(u, cookies)
	c.book.absorb(cookies...)
	if !sameSite(next.Host, c.eps.siteHost()) {
		// Offsite redirect: keep the finding, do not chase it.
		return nil, &offsiteRedirectError{chain: chain.snapshot()}
	}
	c.pinHost(next.Host)
	return next, nil
}
