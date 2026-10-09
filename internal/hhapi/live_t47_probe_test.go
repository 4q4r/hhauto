//go:build live47

package hhapi

// T47 LIVE PROBE (manual; network + owner session required; never in the
// default gate):
//
//	go test -tags live47 -run TestLive47CreateAndDelete -count=1 -v ./internal/hhapi/
//
// Purpose — decide the create-resume contract by live evidence and verify
// the delete endpoint, per the T27/T29 groundwork:
//   - T27 §2.3: POST /profile/shards/resume/create creates a resume and
//     worked from curl/Go (200) on the resume-profile-front BFF; a bodyless
//     create leaves a not_finished shell in the account.
//   - T29: the management delete POST /profile/shards/resume/delete was
//     edge-faked (405) for off-browser TLS clients in September probing —
//     re-probed here against a fresh session.
//
// The probe walks a VARIANT LADDER of create bodies (each carrying a
// unique T47LIVE marker title), diffs the resumes shard after every 2xx to
// detect what actually got created, and DELETES every shell it created
// through the T29 delete endpoint (fallback: DELETE /applicant/resumes/{id})
// — the delete leg doubles as the litter cleanup and the delete-contract
// verdict. The ladder stops at the first variant that produced a TITLED
// shell (the task only needs one accepted shape).
//
// Inputs (never committed, values never printed):
//   - HHAUTO_T47_CONFIG  (default /tmp/opencode/survey/hhauto.toml)
//   - HHAUTO_T47_COOKIES (default /tmp/opencode/hh-cookies.txt)

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

func live47Path(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func live47Client(t *testing.T) (*Client, context.Context) {
	t.Helper()
	cfgPath := live47Path("HHAUTO_T47_CONFIG", "/tmp/opencode/survey/hhauto.toml")
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
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	siteHost := ""
	if u, perr := url.Parse(cfg.API.Endpoints.Site); perr == nil {
		siteHost = u.Host
	}
	if err := c.LoadCookies(ctx, parseCookieHeaderFileT47(t, live47Path("HHAUTO_T47_COOKIES", "/tmp/opencode/hh-cookies.txt"), siteHost)); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c, ctx
}

// parseCookieHeaderFileT47 mirrors the T29 cookie-file loader: one
// "Cookie: k=v; ..." line, rebuilt with parent-domain scope.
func parseCookieHeaderFileT47(t *testing.T, path, siteHost string) []*http.Cookie {
	t.Helper()
	domain := "." + registrableParentT47(siteHost)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cookies file: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	if i := strings.Index(line, "\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
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

func registrableParentT47(host string) string {
	labels := strings.Split(host, ".")
	for len(labels) > 2 {
		labels = labels[1:]
	}
	return strings.Join(labels, ".")
}

// live47Variant is one rung of the create ladder.
type live47Variant struct {
	name string
	url  string
	body string
}

// live47ShardSnapshot maps the current shard by hash for diffing.
func live47ShardSnapshot(t *testing.T, c *Client, ctx context.Context) map[string]Resume {
	t.Helper()
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	out := make(map[string]Resume, len(resumes))
	for _, r := range resumes {
		out[r.HHID] = r
	}
	return out
}

// live47Post fires one raw create variant with the SPA-exact editor header
// set and returns status + body. Redirects are NOT followed (a 3xx on a
// create POST is a verdict, not navigation).
func live47Post(t *testing.T, c *Client, ctx context.Context, target, body string) (int, string) {
	t.Helper()
	spec := requestSpec{
		kind:      kindMutation,
		operation: "live47_probe",
		method:    http.MethodPost,
		url:       target,
		header:    c.cloneResumesHeaders(),
		body:      []byte(body),
	}
	spec.header.Set("Content-Type", "application/json")
	if tok := c.xsrfToken(); tok != "" {
		spec.header.Set("X-XSRFToken", tok)
	}
	resp, err := c.do(ctx, spec)
	if err != nil {
		var se *StatusError
		if errorsAsStatus(err, &se) {
			return se.StatusCode, se.BodySnippet
		}
		t.Logf("POST %s transport error: %v", target, err)
		return 0, ""
	}
	return resp.status, string(resp.body)
}

// live47Dump logs a response without leaking anything session-specific
// (status + first 300 bytes; create envelopes carry no secrets).
func live47Dump(t *testing.T, label string, status int, body string) {
	t.Helper()
	snip := body
	if len(snip) > 300 {
		snip = snip[:300]
	}
	t.Logf("%s -> %d %s", label, status, snip)
}

// TestLive47CreateAndDelete walks the ladder and cleans up after itself.
func TestLive47CreateAndDelete(t *testing.T) {
	c, ctx := live47Client(t)

	// Session liveness gate: if the shard read fails, nothing else is
	// meaningful (and nothing gets created).
	before := live47ShardSnapshot(t, c, ctx)
	t.Logf("session alive; %d resumes in the shard before the probe", len(before))

	bffCreate := "https://resume-profile-front.hh.ru/profile/shards/resume/create"
	siteCreate := c.eps.site + "/profile/shards/resume/create"
	siteShardCreate := c.eps.site + "/shards/resume/create"
	siteApplicantCreate := c.eps.site + "/applicant/resumes/create"

	variants := []live47Variant{
		// V0: T27's verified shell creation (empty body).
		{"V0 bff empty {}", bffCreate, `{"userPrint":{}}`},
		// V1: the clone_resumes creation-properties shape minus resumeHash.
		{"V1 bff creationProperties", bffCreate, fmt.Sprintf(`{"resumesCreationProperties":[{"title":"T47LIVE %d","roleIds":["160"]}],"userPrint":{}}`, time.Now().Unix())},
		// V2: builder-state field names.
		{"V2 bff builder fields", bffCreate, fmt.Sprintf(`{"title":"T47LIVE %d","professionalRole":["160"],"userPrint":{}}`, time.Now().Unix())},
		// V3/V4/V5: the task's candidate routes on the site host.
		{"V3 site /profile/shards/resume/create", siteCreate, fmt.Sprintf(`{"resumesCreationProperties":[{"title":"T47LIVE %d","roleIds":["160"]}],"userPrint":{}}`, time.Now().Unix())},
		{"V4 site /shards/resume/create", siteShardCreate, fmt.Sprintf(`{"resumesCreationProperties":[{"title":"T47LIVE %d","roleIds":["160"]}],"userPrint":{}}`, time.Now().Unix())},
		{"V5 site /applicant/resumes/create", siteApplicantCreate, fmt.Sprintf(`{"resumesCreationProperties":[{"title":"T47LIVE %d","roleIds":["160"]}],"userPrint":{}}`, time.Now().Unix())},
	}

	type createdShell struct {
		hash    string
		title   string
		variant string
	}
	var created []createdShell
	ladderDone := false

	for _, v := range variants {
		if ladderDone {
			break
		}
		status, body := live47Post(t, c, ctx, v.url, v.body)
		live47Dump(t, v.name, status, body)
		if status == 0 {
			continue // transport loss: no evidence, next rung
		}
		if status >= 300 {
			continue // rejected or redirected: next rung
		}
		// Diff the shard: what appeared?
		after := live47ShardSnapshot(t, c, ctx)
		var fresh []Resume
		for h, r := range after {
			if _, ok := before[h]; !ok {
				fresh = append(fresh, r)
			}
		}
		for _, r := range fresh {
			t.Logf("  created: hash=%s title=%q status=%s", r.HHID, r.Title, r.Status)
			created = append(created, createdShell{hash: r.HHID, title: r.Title, variant: v.name})
			if strings.Contains(r.Title, "T47LIVE") {
				ladderDone = true // titled shell: the contract is decided
			}
		}
		if len(fresh) == 0 {
			t.Logf("  2xx but no new shard entry (silent no-op)")
		}
		// Track everything that appeared so cleanup covers it even when a
		// later rung also fires.
		for h := range after {
			before[h] = after[h]
		}
	}

	if len(created) == 0 {
		t.Fatalf("no create variant produced a shard entry; ladder exhausted")
	}

	// Delete leg: every shell this probe created must be removable — the
	// T29 delete first, then the DELETE /applicant/resumes/{id} fallback.
	for _, sh := range created {
		t.Logf("deleting %s (%s, title %q)", sh.hash, sh.variant, sh.title)
		derr := c.DeleteResume(ctx, sh.hash)
		if derr == nil {
			t.Logf("  T29 delete accepted ({url} envelope)")
		} else {
			t.Logf("  T29 delete failed: %v — trying DELETE /applicant/resumes/%s", derr, sh.hash)
			derr = live47RESTDelete(t, c, ctx, sh.hash)
			if derr != nil {
				t.Logf("  REST delete also failed: %v", derr)
				continue
			}
			t.Logf("  REST delete answered 2xx")
		}
		after := live47ShardSnapshot(t, c, ctx)
		if _, still := after[sh.hash]; still {
			t.Errorf("shell %s still present after delete", sh.hash)
		} else {
			t.Logf("  verified gone from the shard")
		}
	}
}

// live47RESTDelete tries the legacy REST shape DELETE /applicant/resumes/{id}.
func live47RESTDelete(t *testing.T, c *Client, ctx context.Context, hash string) error {
	t.Helper()
	spec := requestSpec{
		kind:      kindMutation,
		operation: "live47_rest_delete",
		method:    http.MethodDelete,
		url:       c.eps.site + "/applicant/resumes/" + hash,
		header:    c.xhrHeaders(c.eps.site + "/applicant/resumes"),
	}
	if tok := c.xsrfToken(); tok != "" {
		spec.header.Set("X-XSRFToken", tok)
	}
	resp, err := c.do(ctx, spec)
	if err != nil {
		return err
	}
	live47Dump(t, "DELETE /applicant/resumes/{id}", resp.status, string(resp.body))
	if resp.status >= 300 {
		return fmt.Errorf("status %d", resp.status)
	}
	return nil
}

// errorsAsStatus is a tiny local errors.As wrapper keeping the probe file
// dependency-light (the hhapi package already exports StatusError).
func errorsAsStatus(err error, target **StatusError) bool {
	for err != nil {
		if se, ok := err.(*StatusError); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
