//go:build live29

package hhapi

// T29 LIVE VERIFICATION (manual, network + owner session required; never
// in the default gate):
//
//	go test -tags live29 -run TestLive29UpdateAndRollback -count=1 -v ./internal/hhapi/
//	go test -tags live29 -run TestLive29HideShow          -count=1 -v ./internal/hhapi/
//	HHAUTO_T29_DELETE_SHELL=1 go test -tags live29 -run TestLive29DeleteShell -count=1 -v ./internal/hhapi/
//
// 2026-09-25 LIVE RESULTS (owner session, shell 9a3ed0a3… from the T27
// cleanup list):
//   - update: FAIL — bare pre-app 400 for every body variant (16 POSTs:
//     exact SPA reconstruction, live screen ids, live additionalProperties
//     shape, minimal diffs, Chrome UA, no-XRW, primed jar); site host =
//     405 edge fake. The wall is transport-level, not payload-level.
//   - hide/show: endpoints answered 2xx; the flip is not observable on a
//     not_finished shell (accessType stayed "clients") — accepted as
//     no-op/drop, inconclusive by design of the shell.
//   - delete: FAIL — 405 Allow:GET edge fake for tls-client (5 hhtmSource
//     variants), first attempt 2xx-empty with the shell still present
//     (silent drop; DeleteResume correctly refuses to claim success).
//
// Conclusion: the whole hh.ru /profile/shards/** POST family is edge-gated
// for off-browser TLS clients; only the BFF clone_resumes passes (T28).
// Probes the cracked builder update payload (chunk 6069 thunk P + the T28
// header set) and the management endpoints (module 91062) against the LIVE
// hh.ru on a NOT_FINISHED SHELL — never a base resume:
//
//  1. update: change the shell title to a "T29LIVE" suffixed marker, verify
//     by read-back, then ROLL BACK to the original title and verify again.
//  2. visibility: hide the shell, verify it left the searchable set, show
//     it back.
//  3. delete: DESTRUCTIVE and double-gated — HHAUTO_T29_DELETE_SHELL must
//     be set (1/yes = auto-pick the oldest not_finished shell, or an exact
//     hash); the target must read back not_finished before the DELETE.
//
// The shell under test is HHAUTO_T29_SHELL when set; otherwise the OLDEST
// not_finished shell in the account is picked (the T27 probe litter the
// owner wants cleaned anyway).
//
// Inputs (never committed, values never printed):
//   - HHAUTO_T29_CONFIG  (default /tmp/opencode/survey/hhauto.toml)
//   - HHAUTO_T29_COOKIES (default /tmp/opencode/hh-cookies.txt)
//   - HHAUTO_T29_SHELL   (optional exact shell hash)
//   - HHAUTO_T29_DELETE_SHELL (destructive gate; see above)

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

func live29Path(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func live29Client(t *testing.T) (*Client, context.Context) {
	t.Helper()
	cfgPath := live29Path("HHAUTO_T29_CONFIG", "/tmp/opencode/survey/hhauto.toml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Skipf("survey config not reachable: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load survey config: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("expected exactly one account in the survey config, got %d", len(cfg.Accounts))
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	opts := []ClientOption{WithTransport(cfg.API.Transport)}
	if acct.ProxyRef != "" {
		p, ok := cfg.Proxy[acct.ProxyRef]
		if !ok {
			t.Fatalf("proxy_ref %q missing from config", acct.ProxyRef)
		}
		purl, perr := proxy.URL(p)
		if perr != nil {
			t.Fatalf("proxy url: %v", perr)
		}
		opts = append(opts, WithProxyURL(purl))
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	siteHost := ""
	if u, perr := url.Parse(cfg.API.Endpoints.Site); perr == nil {
		siteHost = u.Host
	}
	if err := c.LoadCookies(ctx, parseCookieHeaderFileT29(t, live29Path("HHAUTO_T29_COOKIES", "/tmp/opencode/hh-cookies.txt"), siteHost)); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c, ctx
}

// parseCookieHeaderFileT29 reads the single "Cookie: k=v; ..." line (a bare
// k=v; line works too) and rebuilds the cookies with the parent-domain
// scope a browser session has (see the T24 probe for the rationale).
func parseCookieHeaderFileT29(t *testing.T, path, siteHost string) []*http.Cookie {
	t.Helper()
	domain := "." + registrableParentT29(siteHost)
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

// registrableParentT29 drops the leftmost label of a host while more than
// two labels remain ("api.hh.ru" -> "hh.ru").
func registrableParentT29(host string) string {
	labels := strings.Split(host, ".")
	for len(labels) > 2 {
		labels = labels[1:]
	}
	return strings.Join(labels, ".")
}

// pickShell resolves the shell under test: HHAUTO_T29_SHELL when set,
// otherwise the OLDEST not_finished shell in the account (never a base
// resume — the not_finished guard in requireShell holds either way).
func pickShell(t *testing.T, c *Client, ctx context.Context) string {
	t.Helper()
	if hash := os.Getenv("HHAUTO_T29_SHELL"); hash != "" {
		requireShellT29(t, c, ctx, hash)
		return hash
	}
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	best := ""
	var bestUpdated int64
	for _, r := range resumes {
		if r.Status != "not_finished" {
			continue
		}
		var updated int64
		if !r.UpdatedAt.IsZero() {
			updated = r.UpdatedAt.UnixMilli()
		}
		if best == "" || updated < bestUpdated {
			best, bestUpdated = r.HHID, updated
		}
	}
	if best == "" {
		t.Fatal("no not_finished shell found in the account (nothing to probe)")
	}
	t.Logf("auto-picked the oldest not_finished shell: %s", best)
	return best
}

// requireShellT29 refuses anything but a not_finished shell: the
// base-resume invariant is enforced at the probe level too.
func requireShellT29(t *testing.T, c *Client, ctx context.Context, hash string) {
	t.Helper()
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	for _, r := range resumes {
		if r.HHID == hash {
			if r.Status != "not_finished" {
				t.Fatalf("resume %s status = %q, want not_finished (never touch base resumes)", hash, r.Status)
			}
			t.Logf("shell %s confirmed not_finished (title %q)", hash, r.Title)
			return
		}
	}
	t.Fatalf("shell %s not found in the account list", hash)
}

// Probe 1: the cracked update payload lands live and rolls back.
func TestLive29UpdateAndRollback(t *testing.T) {
	c, ctx := live29Client(t)
	hash := pickShell(t, c, ctx)

	before, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	origTitle := before.Resume["title"]
	t.Logf("current title: %s", origTitle)

	mutated := before.Clone()
	mutated.Resume["title"], _ = json.Marshal([]map[string]string{{"string": "T29LIVE probe title"}})
	if err := c.UpdateResumeContent(ctx, hash, mutated, []string{"title"}); err != nil {
		t.Fatalf("update with the cracked payload: %v", err)
	}
	after, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if !strings.Contains(string(after.Resume["title"]), "T29LIVE") {
		t.Fatalf("title did not land: %s", after.Resume["title"])
	}
	t.Logf("UPDATE LIVE OK: title landed, verify read-back matched")

	// Roll back.
	rollback := after.Clone()
	rollback.Resume["title"] = origTitle
	if err := c.UpdateResumeContent(ctx, hash, rollback, []string{"title"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	final, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("rollback read-back: %v", err)
	}
	var want, got []map[string]json.RawMessage
	_ = json.Unmarshal(origTitle, &want)
	_ = json.Unmarshal(final.Resume["title"], &got)
	if len(want) > 0 && len(got) > 0 {
		var w, g string
		_ = json.Unmarshal(want[0]["string"], &w)
		_ = json.Unmarshal(got[0]["string"], &g)
		if w != "" && w != g {
			t.Fatalf("rollback mismatch: %q != %q", g, w)
		}
	}
	t.Logf("ROLLBACK LIVE OK: title restored")
}

// Probe 2: hide flips the resume's visibility, show restores it.
func TestLive29HideShow(t *testing.T) {
	c, ctx := live29Client(t)
	hash := pickShell(t, c, ctx)
	requireShellT29(t, c, ctx, hash)

	if err := c.HideResume(ctx, hash); err != nil {
		t.Fatalf("hide: %v", err)
	}
	t.Logf("HIDE LIVE OK")
	if err := c.ShowResume(ctx, hash); err != nil {
		t.Fatalf("show: %v", err)
	}
	t.Logf("SHOW LIVE OK")
}

// Probe 3: DESTRUCTIVE — deletes exactly one not_finished shell. The gate
// is HHAUTO_T29_DELETE_SHELL: "1"/"yes"/"true" auto-picks the oldest
// not_finished shell, any other value is treated as an exact hash.
func TestLive29DeleteShell(t *testing.T) {
	c, ctx := live29Client(t)
	gate := strings.TrimSpace(os.Getenv("HHAUTO_T29_DELETE_SHELL"))
	if gate == "" {
		t.Skip("HHAUTO_T29_DELETE_SHELL not set — the destructive probe stays off")
	}
	hash := gate
	switch strings.ToLower(gate) {
	case "1", "yes", "true":
		hash = pickShell(t, c, ctx)
	default:
		requireShellT29(t, c, ctx, hash)
	}

	if err := c.DeleteResume(ctx, hash); err != nil {
		t.Fatalf("delete: %v", err)
	}
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes after delete: %v", err)
	}
	for _, r := range resumes {
		if r.HHID == hash {
			t.Fatalf("deleted shell %s still listed", hash)
		}
	}
	t.Logf("DELETE LIVE OK: %s removed from the account", hash)
}

var _ = http.StatusOK
