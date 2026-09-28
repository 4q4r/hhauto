package hhapi

// T24: unit tests for the tls-client transport mode. All traffic stays on
// httptest servers — no external network. The live proof lives in
// live_t24_search_test.go (build tag `live`).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hhauto/internal/config"
)

const (
	tlsTestChrome138UA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	tlsTestChrome152UA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	tlsTestFirefox155UA = "Mozilla/5.0 (X11; Linux x86_64; rv:155.0) Gecko/20100101 Firefox/155.0"
	tlsTestFirefox135UA = "Mozilla/5.0 (X11; Linux x86_64; rv:135.0) Gecko/20100101 Firefox/135.0"
)

// newTLSClient builds a tls-client-mode client pointed at one httptest server.
func newTLSClient(t *testing.T, srv string, ua string, opts ...ClientOption) *Client {
	t.Helper()
	return newTLSClientTwin(t, srv, srv, ua, opts...)
}

// newTLSClientTwin points the client at main and allows handlers to redirect
// to the mirror (same IP, different port — sameSite treats them as one site,
// exactly like the hh.ru -> taganrog.hh.ru regional hop).
func newTLSClientTwin(t *testing.T, main, mirror string, ua string, opts ...ClientOption) *Client {
	t.Helper()
	eps := config.APIEndpoints{Site: main, Chatik: main, SearchPage: main + "/search/vacancy"}
	cfg := config.Account{UserAgent: ua, RPS: 500}
	// T50: production tls-client transports always carry the captured
	// browser hello — the tests ride the same path.
	opts = append([]ClientOption{WithTransport(config.TransportTLSClient), WithBrowserTLSSpec(captureSpec(t))}, opts...)
	c, err := NewClient(cfg, eps, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewClient(tls-client): %v", err)
	}
	_ = mirror
	return c
}

func TestProfileForUAPicksClosestAvailable(t *testing.T) {
	// T50: the profile tables feed only the HTTP/2 settings base of the
	// captured-hello transport (browserClientProfile) — the closest
	// lower-or-equal major, no approximation WARN anymore.
	tests := []struct {
		name     string
		ua       string
		wantName string
	}{
		{"chrome exact", tlsTestChrome152UA, "chrome_152"},
		{"chrome 138 has no profile, nearest lower wins", tlsTestChrome138UA, "chrome_133"},
		{"firefox exact", tlsTestFirefox135UA, "firefox_135"},
		{"firefox 155 has no profile, newest wins", tlsTestFirefox155UA, "firefox_148"},
		{"unknown UA falls back to library default", "curl/8.5.0", "chrome_150"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := profileForUA(tt.ua)
			if got.name != tt.wantName {
				t.Fatalf("profileForUA(%q).name = %q, want %q", tt.ua, got.name, tt.wantName)
			}
		})
	}
}

func TestTLSModeRoundTripCarriesHeaders(t *testing.T) {
	var gotUA, gotMethod string
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotMethod = r.Method
		_, _ = w.Write([]byte("pong"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTLSClient(t, srv.URL, tlsTestChrome138UA)
	resp, err := c.do(context.Background(), requestSpec{
		kind: kindGet, operation: "test", method: http.MethodGet,
		url: srv.URL + "/ping", header: http.Header{"User-Agent": []string{tlsTestChrome138UA}},
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.status)
	}
	if string(resp.body) != "pong" {
		t.Fatalf("body = %q, want pong", resp.body)
	}
	if gotUA != tlsTestChrome138UA {
		t.Fatalf("server saw User-Agent %q, want %q", gotUA, tlsTestChrome138UA)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("server saw method %q, want GET", gotMethod)
	}
	if len(resp.chain) != 0 {
		t.Fatalf("chain = %v, want empty", resp.chain)
	}
	if resp.finalURL != srv.URL+"/ping" {
		t.Fatalf("finalURL = %q, want %q", resp.finalURL, srv.URL+"/ping")
	}
}

func TestTLSModeCookiesSurviveAcrossRequests(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/set", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc123", Path: "/"})
		_, _ = w.Write([]byte("set"))
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Cookie"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTLSClient(t, srv.URL, tlsTestChrome138UA)
	ctx := context.Background()
	if _, err := c.do(ctx, requestSpec{kind: kindGet, operation: "set", method: http.MethodGet, url: srv.URL + "/set"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	resp, err := c.do(ctx, requestSpec{kind: kindGet, operation: "echo", method: http.MethodGet, url: srv.URL + "/echo"})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !strings.Contains(string(resp.body), "sid=abc123") {
		t.Fatalf("second request carried Cookie %q, want sid=abc123", resp.body)
	}
	found := false
	for _, ck := range c.Cookies() {
		if ck.Name == "sid" && ck.Value == "abc123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Cookies() snapshot misses sid=abc123: %v", c.Cookies())
	}
}

func TestTLSModeRedirectFollowsPinsAndAbsorbsHopCookies(t *testing.T) {
	mux1 := http.NewServeMux()
	mux2 := http.NewServeMux()
	srv1 := httptest.NewServer(mux1)
	defer srv1.Close()
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()

	var finalCookie, finalHost string
	mux1.HandleFunc("/start", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "hop", Value: "1", Path: "/"})
		w.Header().Set("Location", srv2.URL+"/final")
		w.WriteHeader(http.StatusFound)
	})
	mux2.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		finalCookie = r.Header.Get("Cookie")
		finalHost = r.Host
		_, _ = w.Write([]byte("final"))
	})

	c := newTLSClientTwin(t, srv1.URL, srv2.URL, tlsTestChrome138UA)
	resp, err := c.do(context.Background(), requestSpec{
		kind: kindGet, operation: "test", method: http.MethodGet, url: srv1.URL + "/start",
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.status != http.StatusOK || string(resp.body) != "final" {
		t.Fatalf("final response = (%d, %q), want (200, final)", resp.status, resp.body)
	}
	if !strings.Contains(finalCookie, "hop=1") {
		t.Fatalf("redirect target saw Cookie %q, want hop=1 (hop Set-Cookie must be absorbed)", finalCookie)
	}
	if want := strings.TrimPrefix(srv2.URL, "http://"); finalHost != want {
		t.Fatalf("final hop Host = %q, want %q", finalHost, want)
	}
	if len(resp.chain) != 1 || resp.chain[0].Status != http.StatusFound || resp.chain[0].Location != srv2.URL+"/final" {
		t.Fatalf("chain = %+v, want one 302 hop to %s", resp.chain, srv2.URL+"/final")
	}
	if got := c.effectiveHost(); got != strings.TrimPrefix(srv2.URL, "http://") {
		t.Fatalf("pinned host = %q, want %q (regional pinning)", got, strings.TrimPrefix(srv2.URL, "http://"))
	}
	if resp.finalURL != srv2.URL+"/final" {
		t.Fatalf("finalURL = %q, want %q", resp.finalURL, srv2.URL+"/final")
	}
}

func TestTLSModeRedirectOffsiteIsFenced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/off", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://offsite.example/next")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTLSClient(t, srv.URL, tlsTestChrome138UA)
	_, err := c.do(context.Background(), requestSpec{
		kind: kindGet, operation: "test", method: http.MethodGet, url: srv.URL + "/off",
	})
	if err == nil {
		t.Fatal("offsite redirect must fail the request, got nil error")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T (%v), want *StatusError", err, err)
	}
	if !errors.Is(se.Kind, ErrForbidden) {
		t.Fatalf("Kind = %v, want ErrForbidden", se.Kind)
	}
	if !strings.Contains(err.Error(), "redirect left the site registrable domain") {
		t.Fatalf("err = %v, want offsite fence message", err)
	}
	if len(se.Chain) != 1 {
		t.Fatalf("Chain = %+v, want the single offsite hop", se.Chain)
	}
}

func TestTLSModeRedirectLoopStopsAtBudget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/loop", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTLSClient(t, srv.URL, tlsTestChrome138UA)
	_, err := c.do(context.Background(), requestSpec{
		kind: kindGet, operation: "test", method: http.MethodGet, url: srv.URL + "/loop",
	})
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want redirect-budget stop", err)
	}
}

func TestTLSModePostRedirectSemanticsMatchNetHTTP(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		wantMethod string
	}{
		{"302 downgrades POST to GET", http.StatusFound, http.MethodGet},
		{"307 preserves POST", http.StatusTemporaryRedirect, http.MethodPost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var afterMethod string
			mux := http.NewServeMux()
			mux.HandleFunc("/post", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/after")
				w.WriteHeader(tt.code)
			})
			mux.HandleFunc("/after", func(w http.ResponseWriter, r *http.Request) {
				afterMethod = r.Method
				_, _ = w.Write([]byte("after"))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := newTLSClient(t, srv.URL, tlsTestChrome138UA)
			resp, err := c.do(context.Background(), requestSpec{
				kind: kindMutation, operation: "test", method: http.MethodPost,
				url: srv.URL + "/post", body: []byte("x=1"),
			})
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			if resp.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.status)
			}
			if afterMethod != tt.wantMethod {
				t.Fatalf("follow-up method = %q, want %q", afterMethod, tt.wantMethod)
			}
		})
	}
}
