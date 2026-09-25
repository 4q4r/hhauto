//go:build live30

package autoclone

// T30 LIVE VERIFICATION (manual, network + owner session + Chrome
// required; never in the default gate):
//
//	go test -tags live30 -run TestLive30EditorApply -count=1 -v ./internal/features/autoclone/
//
// 2026-09-25 LIVE RESULT (owner session, oldest not_finished shell
// 9a3ed0a3…): the browser runner opened the wizard title screen with the
// injected session, set the title through the page (browser TLS passes
// the edge that 400s the Go-TLS builder POST — the T29 wall), and the
// get_by_hash read-back verified the marker; the rollback pass verified
// the original title restored. The account shells were left untouched.
//
// Applies the plan to a NOT_FINISHED SHELL — never a base resume:
//
//	1. change the shell title to a "T30LIVE" suffixed marker through the
//	   real Chrome editor pages, verify by read-back;
//	2. roll the original title back the same way, verify again.
//
// Inputs (never committed, values never printed):
//   - HHAUTO_T30_CONFIG  (default /tmp/opencode/survey/hhauto.toml)
//   - HHAUTO_T30_COOKIES (default /tmp/opencode/hh-cookies.txt)
//   - HHAUTO_T30_SHELL   (optional exact shell hash; otherwise the OLDEST
//     not_finished shell in the account)
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/proxy"
	"hhauto/internal/store"
)

func live30Path(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func TestLive30EditorApply(t *testing.T) {
	cfgPath := live30Path("HHAUTO_T30_CONFIG", "/tmp/opencode/survey/hhauto.toml")
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
	acctID := ""
	for id, a := range cfg.Accounts {
		acct, acctID = a, id
	}
	opts := []hhapi.ClientOption{hhapi.WithTransport(cfg.API.Transport)}
	if acct.ProxyRef != "" {
		p, ok := cfg.Proxy[acct.ProxyRef]
		if !ok {
			t.Fatalf("proxy_ref %q missing from config", acct.ProxyRef)
		}
		purl, perr := proxy.URL(p)
		if perr != nil {
			t.Fatalf("proxy url: %v", perr)
		}
		opts = append(opts, hhapi.WithProxyURL(purl))
	}
	c, err := hhapi.NewClient(acct, cfg.API.Endpoints, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	siteHost := ""
	if u, perr := url.Parse(cfg.API.Endpoints.Site); perr == nil {
		siteHost = u.Host
	}
	cookies := live30CookieHeader(t, live30Path("HHAUTO_T30_COOKIES", "/tmp/opencode/hh-cookies.txt"), siteHost)
	if err := c.LoadCookies(ctx, cookies); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	if _, err := findEditorChrome(); err != nil {
		t.Skipf("Chrome not available: %v", err)
	}

	shell := live30PickShell(t, c, ctx)

	// The runner's lineage invariant: register the shell as a tracked
	// clone of itself-shaped parent (a live probe fixture, not a real
	// lineage row the pipeline uses).
	db, err := store.Open(ctx, fmt.Sprintf("%s/live30.db", t.TempDir()), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := db.ResumeClonesRegister(ctx, &store.ResumeClone{
		CloneHash: shell, ParentHash: "live30-parent", Depth: 1, AccountID: acctID,
	}); err != nil {
		t.Fatalf("register probe lineage: %v", err)
	}

	live, err := c.ResumeContent(ctx, shell)
	if err != nil {
		t.Fatalf("live content: %v", err)
	}
	spec, _ := Spec("title")
	raw, ok := live.Resume[spec.Key]
	if !ok {
		t.Fatalf("title missing from the live state")
	}
	original := string(editorNaturalValue(spec, raw))
	marker := live30Marker(original, shell)

	r, err := NewChromeEditorRunner(EditorOptions{
		Cfg:     editorLiveCfg(cfg.API.Endpoints.Site),
		DB:      db,
		Cookies: live30CookieSource{cookies: cookies},
		Verify:  c,
		Log:     testLoggerT(),
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	screen := editorScreensFor("title", live)
	if screen == "" {
		screen = "title"
	}
	t.Logf("shell %s: original title %q, screen %q", shell, original, screen)

	applyTitle := func(value string) {
		t.Helper()
		raw, merr := json.Marshal(value)
		if merr != nil {
			t.Fatalf("marshal title: %v", merr)
		}
		res, aerr := r.ApplyFieldPlan(ctx, shell, []FieldChange{{
			Field: "title", Locator: "resume.title", Screen: screen,
			Kind: KindText, Label: spec.Label, Item: -1, Value: raw,
		}})
		if aerr != nil {
			t.Fatalf("apply %q: %v", value, aerr)
		}
		if len(res.Applied) != 1 || len(res.Failed) != 0 {
			t.Fatalf("apply %q: applied=%v failed=%+v", value, res.Applied, res.Failed)
		}
	}
	applyTitle(marker)
	t.Logf("marker applied and verified by read-back")
	// Roll back only a restorable title: hh requires 2–100 characters, so
	// an empty probe-shell title cannot be restored — the marker stays on
	// this probe shell (documented litter).
	trimmed := strings.TrimSpace(original)
	if len([]rune(trimmed)) >= 2 && !strings.EqualFold(trimmed, "null") {
		applyTitle(original)
		t.Logf("original title restored and verified")
	} else {
		t.Logf("shell title %q is not restorable (hh needs 2-100 chars) — the marker stays on this probe shell", original)
	}
}

func live30Marker(original, shell string) string {
	const maxLen = 100
	base := strings.TrimSpace(original)
	if len([]rune(base)) < 2 || strings.EqualFold(base, "null") {
		n := len(shell)
		if n > 8 {
			n = 8
		}
		base = "T30LIVE " + shell[:n]
	}
	if len([]rune(base)) > maxLen-8 {
		base = string([]rune(base)[:maxLen-8])
	}
	return strings.TrimSpace(base + " T30LIVE")
}

func editorLiveCfg(site string) config.AutoCloneImproveEditor {
	return config.AutoCloneImproveEditor{
		Enabled:        true,
		BaseURL:        site,
		CookieDomain:   "." + registrableParent30(site),
		PageTimeoutSec: 180,
		SaveTimeoutSec: 30,
	}
}

// registrableParent30 drops the leftmost label of a URL host while more
// than two labels remain ("hh.ru" stays, "api.hh.ru" -> "hh.ru").
func registrableParent30(site string) string {
	host := site
	if u, err := url.Parse(site); err == nil && u.Host != "" {
		host = u.Host
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	labels := strings.Split(host, ".")
	for len(labels) > 2 {
		labels = labels[1:]
	}
	return strings.Join(labels, ".")
}

// live30PickShell resolves the shell under test: HHAUTO_T30_SHELL when set
// (must be a not_finished resume), otherwise a not_finished shell whose
// position screen can pass hh validation on save (the professional role
// set) — the runner fills only the planned fields, so a shell missing
// unrelated required values would reject the save (live observation
// 2026-09-26: "Пожалуйста, укажите" on a role-less shell).
func live30PickShell(t *testing.T, c *hhapi.Client, ctx context.Context) string {
	t.Helper()
	if hash := os.Getenv("HHAUTO_T30_SHELL"); hash != "" {
		live30RequireShell(t, c, ctx, hash)
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
		content, cerr := c.ResumeContent(ctx, r.HHID)
		if cerr != nil {
			continue
		}
		var role []map[string]json.RawMessage
		if raw, ok := content.Resume["professionalRole"]; !ok || json.Unmarshal(raw, &role) != nil || len(role) == 0 {
			continue // the position screen cannot save without the role
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
		t.Skip("no not_finished shell with the professional role set — the position screen would reject the save; fill a shell manually and rerun")
	}
	live30RequireShell(t, c, ctx, best)
	return best
}

func live30RequireShell(t *testing.T, c *hhapi.Client, ctx context.Context, hash string) {
	t.Helper()
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("ListResumes: %v", err)
	}
	for _, r := range resumes {
		if r.HHID != hash {
			continue
		}
		if r.Status != "not_finished" {
			t.Fatalf("resume %s is %q — the live runner only touches not_finished shells", hash, r.Status)
		}
		return
	}
	t.Fatalf("resume %s not found in the account", hash)
}

// live30CookieSource hands the file-parsed session to the browser runner.
type live30CookieSource struct {
	cookies []*http.Cookie
}

func (s live30CookieSource) EditorCookies(context.Context, string) ([]*http.Cookie, error) {
	return s.cookies, nil
}

// live30CookieHeader reads the single "Cookie: k=v; ..." line with the
// parent-domain scope (same convention as the T29 probes).
func live30CookieHeader(t *testing.T, path, siteHost string) []*http.Cookie {
	t.Helper()
	labels := strings.Split(siteHost, ".")
	for len(labels) > 2 {
		labels = labels[1:]
	}
	domain := "." + strings.Join(labels, ".")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cookies file not reachable: %v", err)
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
