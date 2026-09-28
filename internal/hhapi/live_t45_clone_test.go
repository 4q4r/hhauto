//go:build live45

package hhapi

// T45 LIVE VERIFICATION (manual, network + owner session required; never
// in the default gate):
//
//	HHAUTO_T45_PARENT=<hash> go test -tags live45 -run TestLive45NativeClonePublishes -count=1 -v ./internal/hhapi/
//
// Proves the T45 contract against LIVE hh.ru: CloneResume rides ONLY the
// native clone_resumes call and the clone reads back COMPLETE — the mine
// shard (GET /shards/applicant/resumes) carries the clone with hh-stamped
// parentResumeId, status new/modified, isSearchable, and the builder state
// is titled, finished and searchable. No shell endpoint, no materialization
// walk.
//
// The parent is HHAUTO_T45_PARENT when set, otherwise the newest
// searchable resume with a title (base resumes are "modified" live).
// The created clone stays in the owner account (same as T28/T46 probes).
//
// Inputs (never committed, values never printed):
//   - HHAUTO_T45_CONFIG  (default /tmp/opencode/survey/hhauto.toml)
//   - HHAUTO_T45_COOKIES (default /tmp/opencode/hh-cookies.txt)
//   - HHAUTO_T45_PARENT  (optional exact parent hash)

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

func live45Path(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func live45Client(t *testing.T) (*Client, context.Context) {
	t.Helper()
	cfgPath := live45Path("HHAUTO_T45_CONFIG", "/tmp/opencode/survey/hhauto.toml")
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
	if err := c.LoadCookies(ctx, parseCookieHeaderFileT45(t, live45Path("HHAUTO_T45_COOKIES", "/tmp/opencode/hh-cookies.txt"), siteHost)); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c, ctx
}

func live45Mask(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}

// parseCookieHeaderFileT45 reads the single "Cookie: k=v; ..." line (a bare
// k=v; line works too) and rebuilds the cookies with the parent-domain
// scope a browser session has (see the T24/T29 probes for the rationale).
func parseCookieHeaderFileT45(t *testing.T, path, siteHost string) []*http.Cookie {
	t.Helper()
	domain := "." + registrableParentT45(siteHost)
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

// registrableParentT45 drops the leftmost label of a host while more than
// two labels remain ("api.hh.ru" -> "hh.ru").
func registrableParentT45(host string) string {
	labels := strings.Split(host, ".")
	for len(labels) > 2 {
		labels = labels[1:]
	}
	return strings.Join(labels, ".")
}

func TestLive45NativeClonePublishes(t *testing.T) {
	c, ctx := live45Client(t)

	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	parent := os.Getenv("HHAUTO_T45_PARENT")
	if parent == "" {
		// The mine shard carries no title field (live shape: _attributes +
		// professionalRole only) — pick the newest searchable, finished
		// resume; the real title rides the builder state.
		for _, r := range resumes {
			if r.Searchable && r.Status != "" && r.Status != "not_finished" {
				parent = r.HHID
				break
			}
		}
	}
	if parent == "" {
		t.Fatal("no searchable titled parent resume found in the shard")
	}
	var parentStatus string
	for _, r := range resumes {
		if r.HHID == parent {
			parentStatus = r.Status
		}
	}
	t.Logf("parent %s (status %q) over %d shard resumes", live45Mask(parent), parentStatus, len(resumes))

	start := time.Now()
	clone, err := c.CloneResume(ctx, parent)
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	t.Logf("CloneResume returned %s in %s", live45Mask(clone), time.Since(start).Round(time.Millisecond))

	// Scope item 5: the mine shard must carry the clone with hh-stamped
	// parentResumeId and a published status (new/modified).
	after, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes(after): %v", err)
	}
	var entry *Resume
	for i := range after {
		if after[i].HHID == clone {
			entry = &after[i]
		}
	}
	if entry == nil {
		t.Fatalf("clone %s is absent from the mine shard (%d resumes)", live45Mask(clone), len(after))
	}
	t.Logf("shard: clone %s parent=%s status=%q searchable=%t created=%s",
		live45Mask(clone), live45Mask(entry.ParentResumeID), entry.Status, entry.Searchable, entry.CreatedAt.Format(time.RFC3339))
	if entry.ParentResumeID == "" {
		t.Fatalf("clone shard entry carries no parentResumeId: %+v", entry)
	}
	// The mine shard carries NO title field at all (live shape) — the title
	// is verified on the builder state below. Status: hh's published family
	// (new/modified, approved for moderated accounts); never a draft.
	if entry.Status == "" || entry.Status == "not_finished" {
		t.Fatalf("clone status = %q, want a published state (new/modified)", entry.Status)
	}
	if !entry.Searchable {
		t.Fatal("clone is not searchable")
	}

	// Builder-state read-back: titled, finished, searchable — the one-call
	// duplicate criterion.
	st, err := c.fetchBuilderState(ctx, clone)
	if err != nil {
		t.Fatalf("builder state: %v", err)
	}
	if st.Title() == "" || st.Status() == "" || st.Status() == "not_finished" || !st.IsSearchable {
		t.Fatalf("builder read-back incomplete: title=%q status=%q searchable=%t", st.Title(), st.Status(), st.IsSearchable)
	}
	fmt.Printf("T45 LIVE PASS: clone=%s parent=%s status=%s title=%q\n",
		live45Mask(clone), live45Mask(parent), st.Status(), st.Title())
}
