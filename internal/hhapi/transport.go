package hhapi

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"hhauto/internal/proxy"
)

// maxRedirects mirrors the browser ceiling; loops beyond it are a failure.
const maxRedirects = 10

// publicSuffixETLD1 wraps publicsuffix.EffectiveTLDPlusOne for sameSite().
func publicSuffixETLD1(host string) (string, error) {
	return publicsuffix.EffectiveTLDPlusOne(host)
}

// newCookieJar returns a cookie jar with public-suffix awareness so
// DDoS-Guard (__ddg1/8/9/10), _xsrf, hhtoken and friends survive redirects
// across regional subdomains and chatik.hh.ru.
func newCookieJar() (*cookiejar.Jar, error) {
	return cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
}

// newHTTPTransport builds a transport around an optional proxy dialer.
// TLS verification is non-negotiable and environment proxies are never
// consulted (Proxy: nil) — traffic must go exactly where the caller points.
func newHTTPTransport(dialer proxy.Dialer) *http.Transport {
	t := &http.Transport{
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       70 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   4,
	}
	if dialer != nil {
		t.DialContext = dialer.DialContext
	}
	return t
}

// redirectChain records the hops a single logical request followed. The
// recorder travels inside the request context; net/http copies the original
// context onto every redirect request, so CheckRedirect can append hops.
type redirectChain struct {
	mu   sync.Mutex
	hops []RedirectHop
}

type redirectChainKey struct{}

func withRedirectChain(ctx context.Context) (context.Context, *redirectChain) {
	rc := &redirectChain{}
	return context.WithValue(ctx, redirectChainKey{}, rc), rc
}

func redirectChainFromContext(ctx context.Context) *redirectChain {
	rc, _ := ctx.Value(redirectChainKey{}).(*redirectChain)
	return rc
}

func (rc *redirectChain) add(status int, location string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.hops = append(rc.hops, RedirectHop{Status: status, Location: location})
}

func (rc *redirectChain) snapshot() []RedirectHop {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]RedirectHop(nil), rc.hops...)
}

// checkRedirect follows redirects while (a) the hop budget lasts and
// (b) each hop stays on the same registrable domain as the configured site
// host. Cross-domain hops stop following (a browser would have navigated
// away; our API client must not chase SSO/offsite links). Every hop pins
// the effective site host so subsequent requests reuse the regional
// subdomain the edge assigned (M0: hh.ru -> taganrog.hh.ru).
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("hhapi: stopped after 10 redirects")
	}
	res := req.Response
	if res == nil {
		return nil
	}
	loc := res.Header.Get("Location")
	if rc := redirectChainFromContext(req.Context()); rc != nil {
		rc.add(res.StatusCode, loc)
	}
	if sameSite(req.URL.Host, c.eps.siteHost()) {
		c.pinHost(req.URL.Host)
	}
	if !sameSite(req.URL.Host, c.eps.siteHost()) {
		// Offsite redirect: keep the response, do not chase it.
		return errors.New("hhapi: redirect leaves the site registrable domain")
	}
	return nil
}
