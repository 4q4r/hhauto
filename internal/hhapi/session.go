package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// --- Session bootstrap and XSRF ---

// BootstrapSession performs the light authenticated GET (/applicant/settings):
// it proves the session is alive and seeds the XSRF token cache. Login
// redirects surface as ErrLoggedOut, challenges as ErrCaptcha.
func (c *Client) BootstrapSession(ctx context.Context) error {
	if err := c.refreshXSRF(ctx); err != nil {
		return err
	}
	if c.xsrfToken() == "" {
		return fmt.Errorf("hhapi: bootstrap: %w (xsrf token not found in page or cookies)", ErrForbidden)
	}
	return nil
}

// refreshXSRF re-reads the settings page and updates the token from the SSR
// state, falling back to the _xsrf cookie the page set.
func (c *Client) refreshXSRF(ctx context.Context) error {
	page, err := c.getDoc(ctx, "bootstrap", c.endpointURL(epSettings, c.eps.siteDefault("/applicant/settings")), "")
	if err != nil {
		return err
	}
	state, _ := initialState(page.body)
	if tok := str(state, "xsrfToken"); tok != "" {
		c.setXSRF(tok)
		return nil
	}
	if tok := c.book.cookie("_xsrf"); tok != "" {
		c.setXSRF(tok)
		return nil
	}
	return nil // no token found: caller decides whether that is fatal
}

func (c *Client) setXSRF(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.xsrf = token
}

// xsrfToken returns the cached token, falling back to the _xsrf cookie.
func (c *Client) xsrfToken() string {
	c.mu.Lock()
	if c.xsrf != "" {
		tok := c.xsrf
		c.mu.Unlock()
		return tok
	}
	c.mu.Unlock()
	if tok := c.book.cookie("_xsrf"); tok != "" {
		c.setXSRF(tok)
		return tok
	}
	return ""
}

// Me parses the session block of the settings SSR page.
func (c *Client) Me(ctx context.Context) (*User, error) {
	page, err := c.getDoc(ctx, "me", c.endpointURL(epSettings, c.eps.siteDefault("/applicant/settings")), "")
	if err != nil {
		return nil, err
	}
	state, _ := initialState(page.body)
	session := obj(state["session"])
	if session == nil {
		session = obj(state["user"])
	}
	u := &User{}
	u.ID = int64(num(session, "userId", "uid", "id"))
	u.Email = str(session, "email")
	u.FullName = firstNonEmpty(str(session, "fullName"), strings.TrimSpace(str(session, "firstName")+" "+str(session, "lastName")), str(session, "name"))
	if u.ID == 0 {
		return nil, fmt.Errorf("hhapi: me: %w (no user id in session state)", ErrNotFound)
	}
	return u, nil
}

// --- Cookies: jar bridge and persistence hook ---

// CookiePersister is called after every response so the caller can persist
// the account session (normally a thin wrapper binding cookies.Store to the
// account ID). Save errors are logged, never fatal: persistence must not
// break the in-flight request.
type CookiePersister interface {
	Save(ctx context.Context, accountID string, cookies []*http.Cookie) error
}

// SetPersister attaches the persistence callback and the account ID it
// should be filed under.
func (c *Client) SetPersister(p CookiePersister, accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persister = p
	c.accountID = accountID
}

// LoadCookies seeds the jar and the persistence snapshot from a previously
// saved session (cookies.Store.Load). Cookies without a Domain attribute
// are filed under the site host.
func (c *Client) LoadCookies(ctx context.Context, cookies []*http.Cookie) error {
	site, err := url.Parse(c.eps.site)
	if err != nil {
		return fmt.Errorf("hhapi: load cookies: bad site URL: %w", err)
	}
	for _, ck := range cookies {
		if ck == nil || ck.Name == "" {
			continue
		}
		host := site.Host
		if ck.Domain != "" {
			host = strings.TrimPrefix(ck.Domain, ".")
		}
		u := &url.URL{Scheme: site.Scheme, Host: host, Path: "/"}
		c.jar.SetCookies(u, []*http.Cookie{ck})
		c.book.absorb(ck)
	}
	c.persistCookies(ctx)
	return nil
}

// Cookies snapshots everything the client currently holds, across hosts
// (hh.ru regional subdomain AND chatik.hh.ru, DDoS-Guard cookies included).
func (c *Client) Cookies() []*http.Cookie { return c.book.snapshot() }

func (c *Client) absorbAndPersist(ctx context.Context, resp *response) {
	c.book.absorb(resp.setCookies...)
	c.persistCookies(ctx)
}

func (c *Client) persistCookies(ctx context.Context) {
	c.mu.Lock()
	p, accountID := c.persister, c.accountID
	c.mu.Unlock()
	if p == nil {
		return
	}
	if err := p.Save(ctx, accountID, c.book.snapshot()); err != nil {
		c.log.Warn("hhapi: cookie persistence failed", "account", accountID, "err", err)
	}
}

// cookieBook is the persistence-side mirror of the jar: net/http/cookiejar
// cannot enumerate all stored cookies, so every Set-Cookie observed on the
// wire is folded into a name|domain|path-keyed map. The snapshot is what
// gets persisted and restored.
type cookieBook struct {
	mu sync.Mutex
	m  map[string]*http.Cookie
}

func (b *cookieBook) key(ck *http.Cookie) string {
	return ck.Name + "|" + strings.TrimPrefix(ck.Domain, ".") + "|" + ck.Path
}

func (b *cookieBook) absorb(cks ...*http.Cookie) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = make(map[string]*http.Cookie, 16)
	}
	for _, ck := range cks {
		if ck == nil || ck.Name == "" {
			continue
		}
		b.m[b.key(ck)] = ck
	}
}

func (b *cookieBook) cookie(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ck := range b.m {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (b *cookieBook) snapshot() []*http.Cookie {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*http.Cookie, 0, len(b.m))
	for _, ck := range b.m {
		cc := *ck
		out = append(out, &cc)
	}
	return out
}

// --- SSR initial-state extraction ---

// initialState pulls the JSON state blob out of an SSR page. HH carries it
// either in <template id="HH-Lux-InitialState"> (HTML-entity-encoded JSON —
// verified against live pages) or in window.__INITIAL_STATE__ = {...}.
// ok is false when neither carrier is present; callers stay tolerant.
func initialState(page []byte) (map[string]json.RawMessage, bool) {
	raw, ok := extractInitialStateJSON(string(page))
	if !ok {
		return nil, false
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, false
	}
	return state, true
}

func extractInitialStateJSON(page string) ([]byte, bool) {
	const tpl = `<template id="HH-Lux-InitialState"`
	if i := strings.Index(page, tpl); i >= 0 {
		rest := page[i+len(tpl):]
		end := strings.Index(rest, "</template>")
		if end >= 0 {
			inner := rest[:end]
			if j := strings.Index(inner, ">"); j >= 0 {
				inner = inner[j+1:]
			}
			return []byte(html.UnescapeString(strings.TrimSpace(inner))), true
		}
	}
	const marker = "window.__INITIAL_STATE__"
	if i := strings.Index(page, marker); i >= 0 {
		if j := strings.Index(page[i:], "{"); j >= 0 {
			if obj, n := scanJSONObject(page[i+j:]); n > 0 {
				return []byte(obj), true
			}
		}
	}
	return nil, false
}

// scanJSONObject extracts the first balanced top-level {...} block, honoring
// string literals and escapes, and returns it with its byte length.
func scanJSONObject(s string) (string, int) {
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		r := s[i]
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == '"':
				inStr = false
			}
		case r == '"':
			inStr = true
		case r == '{':
			depth++
		case r == '}':
			depth--
			if depth == 0 {
				if !utf8.ValidString(s[:i+1]) {
					return "", 0
				}
				return s[:i+1], i + 1
			}
		}
	}
	return "", 0
}

// --- Tolerant JSON accessors (hh SSR shapes drift; parse defensively) ---

func obj(raw json.RawMessage) map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func arr(raw json.RawMessage) []json.RawMessage {
	if raw == nil {
		return nil
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil
	}
	return a
}

// str returns the first key present and string-typed. HH fields sometimes
// arrive as {string: "..."} wrappers; that is unwrapped too.
func str(m map[string]json.RawMessage, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		if wrapped := obj(raw); wrapped != nil {
			if inner := str(wrapped, "string", "text", "name", "value"); inner != "" {
				return inner
			}
		}
	}
	return ""
}

func num(m map[string]json.RawMessage, keys ...string) float64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return f
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if f, e := strconv.ParseFloat(s, 64); e == nil {
				return f
			}
		}
	}
	return 0
}

func boolv(m map[string]json.RawMessage, keys ...string) bool {
	if m == nil {
		return false
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return b
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return strings.EqualFold(s, "true")
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var hhTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05-0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

func parseHHTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range hhTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// timeFrom reads a time field under any of the given key spellings.
func timeFrom(m map[string]json.RawMessage, keys ...string) time.Time {
	return parseHHTime(str(m, keys...))
}

// stateArray resolves state["key"] as either a bare array or {items:[...]}.
func stateArray(state map[string]json.RawMessage, key string) []json.RawMessage {
	raw := state[key]
	if a := arr(raw); a != nil {
		return a
	}
	if m := obj(raw); m != nil {
		if a := arr(m["items"]); a != nil {
			return a
		}
		if a := arr(m["vacancies"]); a != nil {
			return a
		}
	}
	return nil
}

// errNilState is returned when an SSR page lacks the expected state node.
var errNilState = errors.New("hhapi: SSR state node missing")
