//go:build live105

package hhapi

// T105 LIVE PROBE (manual, mutating on the owner's account; never in the
// default gate):
//
//	go test -tags live105 -run TestLive105StateDump       -count=1 -v ./internal/hhapi/
//	go test -tags live105 -run TestLive105LanguageEdit    -count=1 -v ./internal/hhapi/
//	go test -tags live105 -run TestLive105WizardInspect   -count=1 -v ./internal/hhapi/
//	go test -tags live105 -run TestLive105WizardPublish   -count=1 -v ./internal/hhapi/
//	go test -tags live105 -run TestLive105Dedup           -count=1 -v ./internal/hhapi/
//	go test -tags live105 -run TestLive105ShardVerify     -count=1 -v ./internal/hhapi/
//
// Target hashes come from HHAUTO_T105_HASHES (comma-separated). The T104
// method for publishing a created shell: the rpf SPA wizard (hh.ru) driven
// by chromedp — the store session rides a throwaway user-data-dir profile
// (the editor-runner isolation invariant), the wizard's specialization
// picker is completed from the live dictionary and every screen is saved
// with «Сохранить и продолжить» until hh publishes. The wizard binds the
// published resume to every experience pool record (live T99 fact), so
// TestLive105Dedup runs the production DetachDuplicateExperience AFTER the
// wizard pass. Artifacts: /tmp/opencode/t105/.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"hhauto/internal/browser"
	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const live105DumpDir = "/tmp/opencode/t105"

// live105RoleTargets maps the requested titles to the professional role
// ids picked from the live dictionary (dumped 2026-10-10, prof_roles.json:
// 96 «Программист, разработчик», 165 «Дата-сайентист» — the ИТ category's
// AI-direction role).
var live105RoleTargets = map[string][]string{
	"Go":     {"96"},
	"AI/LLM": {"165"},
	"Rust":   {"96"},
}

func live105Hashes(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv("HHAUTO_T105_HASHES")
	if raw == "" {
		t.Fatal("HHAUTO_T105_HASHES is required (comma-separated resume hashes)")
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		t.Fatal("HHAUTO_T105_HASHES parsed empty")
	}
	return out
}

func live105Client(t *testing.T) (*Client, context.CancelFunc) {
	t.Helper()
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	rawCk, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(rawCk, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	if err := c.LoadCookies(ctx, cks); err != nil {
		cancel()
		t.Fatalf("load cookies: %v", err)
	}
	return c, cancel
}

func (c *Client) postLive105(ctx context.Context, url string, body []byte) (*response, error) {
	return c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		h.Set("X-hhtmSource", "resume_partial_edit")
		h.Set("X-hhtmFrom", "")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_edit_probe_t105",
			method:    http.MethodPost,
			url:       url,
			header:    h,
			body:      body,
		}
	})
}

// ---- state dump -------------------------------------------------------

func TestLive105StateDump(t *testing.T) {
	if err := os.MkdirAll(live105DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	c, cancel := live105Client(t)
	defer cancel()
	ctx := context.Background()
	for _, hash := range live105Hashes(t) {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("state %s: %v", hash[:8], err)
		}
		dump := map[string]any{
			"title":   st.Title(),
			"status":  st.Status(),
			"screens": st.Screens,
			"resume":  st.Resume,
			"profile": st.ProfileFields,
		}
		if st.NextIncomplete != nil {
			dump["nextIncompleteScreenId"] = *st.NextIncomplete
		}
		blob, _ := json.MarshalIndent(dump, "", " ")
		p := filepath.Join(live105DumpDir, "state-"+hash[:8]+".json")
		if werr := os.WriteFile(p, blob, 0o644); werr != nil {
			t.Fatalf("write %s: %v", p, werr)
		}
		var role []any
		_ = json.Unmarshal(st.Resume["professionalRole"], &role)
		t.Logf("%s: title=%q status=%q nextIncomplete=%v profRole=%v lang=%s",
			hash[:8], st.Title(), st.Status(), st.NextIncomplete, role, string(st.Resume["language"]))
	}
}

// ---- T51 language edit (the T87 contract, T105 targets) ---------------

type live105Lang struct {
	Degree string `json:"degree"`
	ID     int64  `json:"id"`
}

func live105ParseLangs(raw json.RawMessage) []live105Lang {
	var out []live105Lang
	_ = json.Unmarshal(raw, &out)
	return out
}

func live105LangsEqual(a, b []live105Lang) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, l := range a {
		seen[l.Degree+":"+strconv.FormatInt(l.ID, 10)]++
	}
	for _, l := range b {
		k := l.Degree + ":" + strconv.FormatInt(l.ID, 10)
		seen[k]--
		if seen[k] == 0 {
			delete(seen, k)
		}
	}
	return len(seen) == 0
}

func TestLive105LanguageEdit(t *testing.T) {
	if err := os.MkdirAll(live105DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	c, cancel := live105Client(t)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel2()

	target := []live105Lang{{Degree: "l1", ID: 34}, {Degree: "b2", ID: 57}} // T87 contract: Русский native + Английский B2
	targetRaw, _ := json.Marshal(target)
	body, _ := json.Marshal(map[string]json.RawMessage{"language": json.RawMessage(targetRaw)})

	editURL := func(hash string) string {
		return c.eps.siteDefault("/applicant/resume/edit") + "?resume=" + hash + "&hhtmSource=resume_partial_edit"
	}

	for _, hash := range live105Hashes(t) {
		pre, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("pre-read %s: %v", hash[:8], err)
		}
		preTitle := pre.Title()
		if live105LangsEqual(live105ParseLangs(pre.Resume["language"]), target) {
			t.Logf("%s: language already set — skipping POST", hash[:8])
			continue
		}
		resp, err := c.postLive105(ctx, editURL(hash), body)
		if err != nil {
			t.Fatalf("edit %s: transport: %v", hash[:8], err)
		}
		var envelope struct {
			Errors []struct {
				Field string `json:"field"`
				Code  string `json:"code"`
			} `json:"errors"`
		}
		if json.Unmarshal(resp.body, &envelope) == nil && len(envelope.Errors) > 0 {
			t.Fatalf("edit %s: hh rejected: %+v (body %s)", hash[:8], envelope.Errors, snippet(resp.body))
		}
		// Re-read with a poll budget (T55 silent no-op trap).
		var after *builderState
		landed := false
		for attempt := 0; attempt < 6 && !landed; attempt++ {
			if attempt > 0 {
				time.Sleep(5 * time.Second)
			}
			after, err = c.fetchBuilderState(ctx, hash)
			if err != nil {
				t.Fatalf("re-read %s: %v", hash[:8], err)
			}
			landed = live105LangsEqual(live105ParseLangs(after.Resume["language"]), target)
		}
		if !landed {
			t.Fatalf("edit %s: language did not land: %s (payload %s)", hash[:8], string(after.Resume["language"]), string(body))
		}
		if after.Title() != preTitle {
			t.Fatalf("edit %s: TITLE CHANGED %q -> %q", hash[:8], preTitle, after.Title())
		}
		t.Logf("%s: language ✓ %s title ✓ %q", hash[:8], string(after.Resume["language"]), after.Title())
		time.Sleep(3 * time.Second)
	}
}

// ---- chromedp session boilerplate (editor-runner isolation contract) --

func live105Browser(t *testing.T) (ctx context.Context, cancel func(), cookies []*http.Cookie) {
	t.Helper()
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	rawCk, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name     string `json:"name"`
			Value    string `json:"value"`
			Domain   string `json:"domain"`
			Path     string `json:"path"`
			Secure   bool   `json:"secure"`
			HttpOnly bool   `json:"httponly"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(rawCk, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	domain := "hh.ru"
	for _, ck := range blob.Cookies {
		d := strings.TrimPrefix(ck.Domain, ".")
		if strings.HasSuffix(d, domain) || d == "" {
			cookies = append(cookies, &http.Cookie{
				Name: ck.Name, Value: ck.Value, Domain: ck.Domain, Path: ck.Path,
				Secure: ck.Secure, HttpOnly: ck.HttpOnly,
			})
		}
	}
	if len(cookies) == 0 {
		t.Fatal("no hh.ru cookies in the store")
	}
	provider := browser.NewFromConfig(cfg.Browser, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	chromePath, cleanupChrome, err := provider.Chrome(context.Background())
	if err != nil {
		t.Fatalf("chrome: %v", err)
	}
	dir, err := os.MkdirTemp("", "hhauto-t105-")
	if err != nil {
		cleanupChrome()
		t.Fatalf("temp profile: %v", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(dir),
	)
	flags, ferr := browser.StealthFlags(cfg.Browser.Preset)
	if ferr != nil {
		flags = nil
	}
	for _, f := range flags {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	for _, f := range browser.SpecUserAgentFlags(spec.UserAgent) {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	runCtx, cancelRun := context.WithTimeout(browserCtx, 30*time.Minute)
	cancel = func() {
		cancelRun()
		cancelBrowser()
		cancelAlloc()
		cleanupChrome()
		_ = os.RemoveAll(dir)
	}
	// Inject the session before the first navigation (editor-runner
	// invariant: only the configured hh domain ever sees the cookies).
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range cookies {
			dom := c.Domain
			if dom == "" {
				dom = domain
			}
			path := c.Path
			if path == "" {
				path = "/"
			}
			p := network.SetCookie(c.Name, c.Value).
				WithDomain(dom).
				WithPath(path).
				WithSecure(c.Secure || true)
			if c.HttpOnly {
				p = p.WithHTTPOnly(true)
			}
			if err := p.Do(ctx); err != nil {
				return fmt.Errorf("set cookie %s: %w", c.Name, err)
			}
		}
		return nil
	})); err != nil {
		cancel()
		t.Fatalf("inject cookies: %v", err)
	}
	return runCtx, cancel, cookies
}

// ---- wizard inspect (read-only DOM dump) ------------------------------

// titleOf reads one resume's live title through the builder state.
func titleOf(t *testing.T, hash string) string {
	t.Helper()
	c, cancel := live105Client(t)
	defer cancel()
	st, err := c.fetchBuilderState(context.Background(), hash)
	if err != nil {
		t.Fatalf("title read %s: %v", hash[:8], err)
	}
	return st.Title()
}

func TestLive105WizardInspect(t *testing.T) {
	if err := os.MkdirAll(live105DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	bctx, cancel, _ := live105Browser(t)
	defer cancel()
	for _, hash := range live105Hashes(t) {
		title := titleOf(t, hash)
		var raw string
		url := "https://hh.ru/profile/resume?resume=" + hash
		if err := chromedp.Run(bctx,
			chromedp.Navigate(url),
			chromedp.Sleep(6*time.Second),
			// The cookies informer overlays the page — accept it once.
			chromedp.Evaluate(live105ClickQaJS+`("cookies-policy-informer-accept")`, &raw),
			chromedp.Sleep(time.Second),
			// «Укажу профессию» opens the specialization picker.
			chromedp.Evaluate(live105ClickQaJS+`("resume-profile-card-select-job")`, &raw),
			chromedp.Sleep(5*time.Second),
			// Click the title card, then dump the FULL picker state: the
			// position choice may surface spec options on the same screen.
			chromedp.Evaluate(live105ClickChipByValueJS+"("+jsArg(title)+")", &raw),
			chromedp.Sleep(3*time.Second),
			chromedp.Evaluate(live105WizardDumpJS, &raw),
		); err != nil {
			t.Fatalf("inspect %s: %v", hash[:8], err)
		}
		t.Logf("picker after card click %s:\n%s", hash[:8], raw)
		_ = os.WriteFile(filepath.Join(live105DumpDir, "picker-probe-"+hash[:8]+".json"), []byte(raw), 0o644)

		// Click next-screen and capture the reaction (validation errors,
		// state change).
		if err := chromedp.Run(bctx,
			chromedp.Evaluate(live105ClickQaJS+`("resume-profile-next-screen")`, &raw),
			chromedp.Sleep(4*time.Second),
			chromedp.Evaluate(live105WizardDumpJS, &raw),
		); err != nil {
			t.Fatalf("inspect next %s: %v", hash[:8], err)
		}
		t.Logf("after next-screen %s:\n%s", hash[:8], raw)
		_ = os.WriteFile(filepath.Join(live105DumpDir, "picker-after-next-"+hash[:8]+".json"), []byte(raw), 0o644)
	}
}

// live105ChipProbeJS dumps the chip area around the resume title: the
// chip's element tree, classes and ARIA state (read-only).
const live105ChipProbeJS = `((want) => {
  const hits = [];
  for (const el of document.querySelectorAll('label, div, span, button')) {
    const r = el.getBoundingClientRect();
    if (r.width < 10 || r.height < 5) continue;
    if (el.children.length > 6) continue;
    const t = (el.innerText || '').replace(/\s+/g, ' ').trim();
    if (t === want) {
      hits.push({
        tag: el.tagName.toLowerCase(),
        cls: (el.className || '').toString().slice(0, 120),
        qa: el.getAttribute('data-qa') || '',
        aria: el.getAttribute('aria-checked') || el.getAttribute('aria-selected') || el.getAttribute('aria-pressed') || '',
        forId: el.getAttribute('for') || '',
        parentCls: el.parentElement ? (el.parentElement.className || '').toString().slice(0, 120) : '',
        parentQa: el.parentElement ? (el.parentElement.getAttribute('data-qa') || '') : '',
      });
      if (hits.length >= 6) break;
    }
  }
  return JSON.stringify({want, hits, url: location.href});
})`

// ---- wizard publish (the T104 click-through) --------------------------

// live105QaQ returns the query selector for a data-qa value.
func live105QaQ(qa string) string { return `[data-qa="` + qa + `"]` }

// live105WizardDumpJS collects the wizard's interactive skeleton: buttons,
// inputs, role list items and visible section titles.
const live105WizardDumpJS = `(() => {
  const cut = s => (s || '').replace(/\s+/g, ' ').trim().slice(0, 120);
  const out = {location: location.href, title: document.title, buttons: [], inputs: [], texts: [], items: []};
  for (const b of document.querySelectorAll('button, [role="button"], a[href]')) {
    const r = b.getBoundingClientRect();
    if (r.width < 10 || r.height < 5) continue;
    const t = cut(b.innerText) || cut(b.getAttribute('aria-label')) || cut(b.getAttribute('data-qa'));
    if (t) out.buttons.push({t, qa: b.getAttribute('data-qa') || '', tag: b.tagName.toLowerCase()});
  }
  for (const i of document.querySelectorAll('input, textarea, [contenteditable="true"]')) {
    const r = i.getBoundingClientRect();
    if (r.width < 10) continue;
    out.inputs.push({qa: i.getAttribute('data-qa') || '', ph: cut(i.getAttribute('placeholder')),
      type: i.getAttribute('type') || '', val: cut(i.value), label: cut(i.labels && i.labels[0] && i.labels[0].innerText)});
  }
  const main = document.querySelector('main') || document.body;
  for (const el of main.querySelectorAll('h1, h2, h3, [class*="title"], [class*="header"]')) {
    const t = cut(el.innerText);
    if (t && t.length > 2 && !out.texts.includes(t)) out.texts.push(t);
    if (out.texts.length > 25) break;
  }
  for (const li of document.querySelectorAll('[data-qa*="item"], [class*="option"], li[role="option"], [role="checkbox"], [role="menuitem"]')) {
    const r = li.getBoundingClientRect();
    if (r.width < 10 || r.height < 5) continue;
    const t = cut(li.innerText);
    if (t) out.items.push({t, qa: li.getAttribute('data-qa') || ''});
    if (out.items.length > 60) break;
  }
  return JSON.stringify(out);
})()`

// live105ClickQaJS clicks the first visible element matching a data-qa
// value; returns the clicked qa or "none".
const live105ClickQaJS = `((qa) => {
  const els = [...document.querySelectorAll('[data-qa="' + qa + '"]')].filter(e => {
    const r = e.getBoundingClientRect();
    return r.width >= 10 && r.height >= 5;
  });
  if (!els.length) return 'none';
  els[0].click();
  return qa;
})`

// live105ClickButtonByTextJS clicks the first visible button whose text
// equals one of the wanted labels; returns the label or "none".
const live105ClickButtonByTextJS = `((...wants) => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const buttons = [...document.querySelectorAll('button, [role="button"]')].filter(b => {
    const r = b.getBoundingClientRect();
    return r.width >= 10 && r.height >= 5;
  });
  for (const want of wants) {
    const w = norm(want);
    for (const b of buttons) {
      if (norm(b.innerText) === w) { b.click(); return want; }
    }
  }
  return 'none';
})`

// live105ClickChipByValueJS clicks the visible Magritte card (a label
// wrapping the hidden radio) whose text equals the wanted title; reports
// the resulting radio state.
const live105ClickChipByValueJS = `((want) => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  let card = null;
  for (const el of document.querySelectorAll('label')) {
    const r = el.getBoundingClientRect();
    if (r.width < 30 || r.height < 20) continue;
    if (norm(el.innerText) !== want) continue;
    if ((el.className || '').includes('magritte-card')) { card = el; break; }
    if (card === null) card = el;
  }
  if (!card) return 'no-card';
  card.click();
  const state = {clicked: true};
  const scope = card.closest('label') || card;
  const radios = document.querySelectorAll('input[type="radio"]');
  for (const r of radios) {
    if (r.checked && r.value === want) { state.radioChecked = true; break; }
  }
  state.aria = card.getAttribute('aria-checked') || card.getAttribute('aria-pressed') || '';
  return JSON.stringify(state);
})`

// live105TypeQaJS types text into the input with the given data-qa and
// dispatches the input events React needs.
const live105TypeQaJS = `((qa, text) => {
  const e = document.querySelector('[data-qa="' + qa + '"]');
  if (!e) return 'none';
  e.focus();
  const setter = Object.getOwnPropertyDescriptor(e.__proto__, 'value').set;
  setter.call(e, '');
  setter.call(e, text);
  e.dispatchEvent(new Event('input', {bubbles: true}));
  e.dispatchEvent(new Event('change', {bubbles: true}));
  return 'ok';
})`

// live105ClickItemByTextJS clicks the first visible option item whose text
// equals or starts with the wanted name.
const live105ClickItemByTextJS = `((want) => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const w = norm(want);
  const els = [...document.querySelectorAll('[data-qa*="option"], [data-qa*="item"], li[role="option"], [role="option"], [class*="dropdown"] li, [class*="suggest"] li')].filter(e => {
    const r = e.getBoundingClientRect();
    return r.width >= 10 && r.height >= 5;
  });
  for (const e of els) {
    if (norm(e.innerText) === w || norm(e.innerText).startsWith(w)) { e.click(); return norm(e.innerText).slice(0, 60); }
  }
  for (const e of els) {
    if (norm(e.innerText).includes(w)) { e.click(); return norm(e.innerText).slice(0, 60); }
  }
  return 'none';
})`

func TestLive105WizardPublish(t *testing.T) {
	if err := os.MkdirAll(live105DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	c, cancelClient := live105Client(t)
	defer cancelClient()
	apiCtx := context.Background()

	bctx, cancelBrowser, _ := live105Browser(t)
	defer cancelBrowser()

	shardStatus := func(hash string) (string, string) {
		st, err := c.fetchBuilderState(apiCtx, hash)
		if err != nil {
			t.Fatalf("shard read %s: %v", hash[:8], err)
		}
		return st.Status(), st.Title()
	}

	for _, hash := range live105Hashes(t) {
		status, title := shardStatus(hash)
		if status != "not_finished" {
			t.Logf("%s: already published (%s) — skipping wizard", hash[:8], status)
			continue
		}
		t.Logf("%s: publishing %q", hash[:8], title)

		url := "https://hh.ru/profile/resume?resume=" + hash
		if err := chromedp.Run(bctx, chromedp.Navigate(url), chromedp.Sleep(6*time.Second)); err != nil {
			t.Fatalf("navigate %s: %v", hash[:8], err)
		}
		var clicked string
		if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickQaJS+`("cookies-policy-informer-accept")`, &clicked)); err != nil {
			t.Fatalf("cookies informer: %v", err)
		}

		published := false
		for step := 0; step < 25 && !published; step++ {
			var raw string
			if err := chromedp.Run(bctx, chromedp.Evaluate(live105WizardDumpJS, &raw)); err != nil {
				t.Fatalf("dump %s step %d: %v", hash[:8], step, err)
			}
			var d struct {
				Href    string `json:"location"`
				Heading string `json:"title"`
				Buttons []struct {
					Text string `json:"t"`
					QA   string `json:"qa"`
				} `json:"buttons"`
				Inputs []struct {
					QA   string `json:"qa"`
					Type string `json:"type"`
					Val  string `json:"val"`
				} `json:"inputs"`
				Texts []string `json:"texts"`
			}
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				t.Fatalf("dump decode %s: %v", hash[:8], err)
			}
			head := ""
			if len(d.Texts) > 0 {
				head = d.Texts[0]
			}
			t.Logf("step %d: %s | screen=%q", step, d.Href, head)

			// Screen 1: «Кем вы хотите работать?» — open the picker.
			if strings.Contains(head, "Кем вы хотите работать") {
				if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickQaJS+`("resume-profile-card-select-job")`, &clicked)); err != nil || clicked == "none" {
					t.Fatalf("select-job click failed: %v (%s)", err, clicked)
				}
				time.Sleep(4 * time.Second)
				continue
			}

			// The specialization picker: the wizard pre-selects the
			// position from the resume title (the title chip); fall back
			// to typing the dictionary role name.
			hasPickerInput := false
			for _, in := range d.Inputs {
				if in.QA == "resume-profile-position-input" {
					hasPickerInput = true
					break
				}
			}
			if hasPickerInput {
				var picked string
				perr := chromedp.Run(bctx, chromedp.Evaluate(live105ClickChipByValueJS+"("+jsArg(title)+")", &picked))
				if perr != nil || picked == "no-card" || picked == "none" {
					t.Logf("title card not clickable (%v, %s) — typing the role name", perr, picked)
					for _, roleName := range live105RolesForTitle(t, title) {
						var typed string
						if err := chromedp.Run(bctx, chromedp.Evaluate(live105TypeQaJS+"("+jsArg("resume-profile-position-input")+", "+jsArg(roleName)+")", &typed)); err != nil || typed != "ok" {
							t.Fatalf("type into picker: %v (%s)", err, typed)
						}
						time.Sleep(2500 * time.Millisecond)
						var hit string
						if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickItemByTextJS+"("+jsArg(roleName)+")", &hit)); err == nil && hit != "none" {
							picked = "typed:" + hit
							break
						}
					}
				}
				if picked == "" || picked == "none" || picked == "no-card" {
					t.Fatalf("%s: the role picker accepted nothing (title card + dictionary fallback failed)", hash[:8])
				}
				t.Logf("step %d: position picked (%s)", step, picked)
				time.Sleep(3500 * time.Millisecond)

				// «Сохранить и продолжить» on this screen opens the
				// category-tree modal (the specialization picker). The
				// modal renders asynchronously — poll, and give the
				// selection a second nudge when the first save was
				// absorbed before the React state settled.
				openModal := func() string {
					if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickQaJS+`("resume-profile-next-screen")`, &clicked)); err != nil {
						t.Fatalf("next-screen (modal open) %s: %v", hash[:8], err)
					}
					var up string
					for try := 0; try < 8 && up != "ok"; try++ {
						time.Sleep(time.Second)
						if err := chromedp.Run(bctx, chromedp.Evaluate(
							`(() => !!document.querySelector('[data-qa="tree-selector-search-input"]') ? 'ok' : 'wait')()`, &up)); err != nil {
							t.Fatalf("modal poll: %v", err)
						}
					}
					return up
				}
				if up := openModal(); up != "ok" {
					t.Logf("modal did not open on the first save — nudging again")
					if up := openModal(); up != "ok" {
						var stuck string
						_ = chromedp.Run(bctx, chromedp.Evaluate(live105WizardDumpJS, &stuck))
						p := filepath.Join(live105DumpDir, "publish-stuck-"+hash[:8]+"-step"+strconv.Itoa(step)+".json")
						_ = os.WriteFile(p, []byte(stuck), 0o644)
						var radioState string
						_ = chromedp.Run(bctx, chromedp.Evaluate(`(() => JSON.stringify([...document.querySelectorAll('input[type=radio]')].filter(r=>r.checked).map(r=>r.value.slice(0,60))))()`, &radioState))
						t.Fatalf("%s: the category tree modal did not open (checked radios: %s; dumped %s)", hash[:8], radioState, p)
					}
				}
				for _, roleName := range live105RolesForTitle(t, title) {
					// The tree is virtualized (only the first categories
					// render until scrolled) — find the role through the
					// modal's search input.
					var typed string
					if err := chromedp.Run(bctx, chromedp.Evaluate(live105TypeQaJS+"("+jsArg("tree-selector-search-input")+", "+jsArg(roleName)+")", &typed)); err != nil || typed != "ok" {
						t.Fatalf("type into the tree search: %v (%s)", err, typed)
					}
					time.Sleep(3 * time.Second)
					var hit string
					if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickItemByTextJS+"("+jsArg(roleName)+")", &hit)); err != nil || hit == "none" {
						t.Fatalf("role %q not found in the tree: %v (%s)", roleName, err, hit)
					}
					t.Logf("step %d: role %q selected in the tree (%s)", step, roleName, hit)
					time.Sleep(800 * time.Millisecond)
				}
				if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickQaJS+`("category-modal-submit")`, &clicked)); err != nil {
					t.Fatalf("modal submit: %v", err)
				}
				if clicked == "none" {
					// hh may close the modal on the role click itself —
					// not fatal, the walk continues.
					t.Logf("step %d: modal submit not found (modal closed by hh?)", step)
				} else {
					t.Logf("step %d: modal submitted", step)
				}
				time.Sleep(4 * time.Second)

				if st, _ := shardStatus(hash); st != "not_finished" {
					published = true
					t.Logf("%s: PUBLISHED after step %d (status=%s)", hash[:8], step, st)
				}
				continue
			}

			// Universal screen save: «Сохранить и продолжить».
			if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickQaJS+`("resume-profile-next-screen")`, &clicked)); err != nil {
				t.Fatalf("next-screen click %s step %d: %v", hash[:8], step, err)
			}
			t.Logf("step %d: clicked next-screen=%q", step, clicked)
			if clicked == "none" {
				// The terminal screens speak their own labels
				// («Опубликовать»/«Завершить»); try the text fallback.
				var byText string
				if err := chromedp.Run(bctx, chromedp.Evaluate(live105ClickButtonByTextJS+"("+jsArg("Опубликовать")+","+jsArg("Завершить")+","+jsArg("Готово")+")", &byText)); err != nil {
					t.Fatalf("publish-button click %s: %v", hash[:8], err)
				}
				if byText == "none" {
					// No save control at all: either hh finished (the
					// published banner) or an unexpected screen — dump it.
					var stuck string
					_ = chromedp.Run(bctx, chromedp.Evaluate(live105WizardDumpJS, &stuck))
					p := filepath.Join(live105DumpDir, "publish-stuck-"+hash[:8]+"-step"+strconv.Itoa(step)+".json")
					_ = os.WriteFile(p, []byte(stuck), 0o644)
					t.Logf("no publish control; dumped %s", p)
				} else {
					t.Logf("step %d: clicked by text %q", step, byText)
				}
			}
			time.Sleep(4 * time.Second)

			if st, _ := shardStatus(hash); st != "not_finished" {
				published = true
				t.Logf("%s: PUBLISHED after step %d (status=%s)", hash[:8], step, st)
			}
		}

		final, ftitle := shardStatus(hash)
		t.Logf("%s: final status=%q title=%q", hash[:8], final, ftitle)
		if final == "not_finished" {
			t.Errorf("%s: not published after the wizard pass", hash[:8])
		}
	}
}

// jsArg wraps a Go string as a JavaScript string literal argument for
// chromedp.Evaluate payloads of the form `((arg) => {...})` + literal.
func jsArg(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// live105RolesForTitle maps the resume's own title to the wizard role
// names (the live dictionary names behind the target ids).
func live105RolesForTitle(t *testing.T, title string) []string {
	t.Helper()
	lower := strings.ToLower(title)
	switch {
	// The AI check precedes the Go one: the AI/LLM title carries
	// «Go» inside its tech list («Python, Go, FastAPI»).
	case strings.Contains(lower, "ai") || strings.Contains(lower, "llm") || strings.Contains(lower, "ml") || strings.Contains(lower, "дата") || strings.Contains(lower, "data"):
		return []string{"Дата-сайентист"}
	case strings.Contains(lower, "go") || strings.Contains(lower, "golang"):
		return []string{"Программист, разработчик"}
	case strings.Contains(lower, "rust"):
		return []string{"Программист, разработчик"}
	default:
		return []string{"Программист, разработчик"}
	}
}

// ---- T51 professionalRole edit ----------------------------------------

// TestLive105RoleEdit re-points one resume's professional role through the
// T51 edit contract ({"professionalRole":[id]}) — the wizard owns the role
// at publish time, so a post-publish correction rides the same save route.
// The target id rides HHAUTO_T105_ROLE (flat dictionary id).
func TestLive105RoleEdit(t *testing.T) {
	roleID := strings.TrimSpace(os.Getenv("HHAUTO_T105_ROLE"))
	if roleID == "" {
		t.Fatal("HHAUTO_T105_ROLE is required (flat professional role id)")
	}
	if err := os.MkdirAll(live105DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	c, cancel := live105Client(t)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel2()

	body, _ := json.Marshal(map[string]json.RawMessage{
		"professionalRole": json.RawMessage(`["` + roleID + `"]`),
	})
	editURL := func(hash string) string {
		return c.eps.siteDefault("/applicant/resume/edit") + "?resume=" + hash + "&hhtmSource=resume_partial_edit"
	}

	for _, hash := range live105Hashes(t) {
		pre, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("pre-read %s: %v", hash[:8], err)
		}
		preTitle := pre.Title()
		resp, err := c.postLive105(ctx, editURL(hash), body)
		if err != nil {
			t.Fatalf("edit %s: transport: %v", hash[:8], err)
		}
		var envelope struct {
			Errors []struct {
				Field string `json:"field"`
				Code  string `json:"code"`
			} `json:"errors"`
		}
		if json.Unmarshal(resp.body, &envelope) == nil && len(envelope.Errors) > 0 {
			t.Fatalf("edit %s: hh rejected: %+v (body %s)", hash[:8], envelope.Errors, snippet(resp.body))
		}
		// Re-read with a poll budget (T55 silent no-op trap).
		var after *builderState
		landed := false
		for attempt := 0; attempt < 6 && !landed; attempt++ {
			if attempt > 0 {
				time.Sleep(5 * time.Second)
			}
			after, err = c.fetchBuilderState(ctx, hash)
			if err != nil {
				t.Fatalf("re-read %s: %v", hash[:8], err)
			}
			landed = strings.Contains(string(after.Resume["professionalRole"]), roleID)
		}
		if !landed {
			t.Fatalf("edit %s: professionalRole did not land: %s (payload %s)", hash[:8], string(after.Resume["professionalRole"]), string(body))
		}
		if after.Title() != preTitle {
			t.Fatalf("edit %s: TITLE CHANGED %q -> %q", hash[:8], preTitle, after.Title())
		}
		t.Logf("%s: professionalRole ✓ %s (title ✓ %q)", hash[:8], string(after.Resume["professionalRole"]), after.Title())
		time.Sleep(3 * time.Second)
	}
}

// ---- T99 dedup (post-publish render-set pruning) ----------------------

func TestLive105Dedup(t *testing.T) {
	if err := os.MkdirAll(live105DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	c, cancel := live105Client(t)
	defer cancel()
	ctx := context.Background()
	for _, hash := range live105Hashes(t) {
		res, err := c.DetachDuplicateExperience(ctx, hash)
		if err != nil {
			t.Fatalf("dedup %s: %v (state stays consistent; rerun continues)", hash[:8], err)
		}
		blob, _ := json.MarshalIndent(res, "", " ")
		p := filepath.Join(live105DumpDir, "dedup-"+hash[:8]+".json")
		_ = os.WriteFile(p, blob, 0o644)
		t.Logf("dedup %s: kept=%v detached=%v", hash[:8], res.Kept, res.Detached)
	}
}

// ---- shard verification ------------------------------------------------

func TestLive105ShardVerify(t *testing.T) {
	c, cancel := live105Client(t)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel2()
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("shard: %v", err)
	}
	t.Logf("shard: %d resumes", len(resumes))
	for _, r := range resumes {
		t.Logf("  %s  %-24q status=%-10s searchable=%-5v next=%v",
			r.HHID[:8], r.Title, r.Status, r.Searchable, r.UpdatedAt.Format("01-02 15:04"))
	}
	for _, hash := range live105Hashes(t) {
		var found *Resume
		for i := range resumes {
			if resumes[i].HHID == hash {
				found = &resumes[i]
				break
			}
		}
		if found == nil {
			t.Errorf("%s: NOT in the shard", hash[:8])
			continue
		}
		if found.Status == "not_finished" {
			t.Errorf("%s: status=%q, want published (modified)", hash[:8], found.Status)
		}
		if !found.Searchable {
			t.Errorf("%s: not searchable", hash[:8])
		}
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("state %s: %v", hash[:8], err)
		}
		if st.NextIncomplete != nil {
			t.Errorf("%s: nextIncompleteScreenId=%v, want nil", hash[:8], *st.NextIncomplete)
		}
		var langs []live105Lang
		_ = json.Unmarshal(st.Resume["language"], &langs)
		want := []live105Lang{{Degree: "l1", ID: 34}, {Degree: "b2", ID: 57}}
		if !live105LangsEqual(langs, want) {
			t.Errorf("%s: language=%s, want [l1:34 b2:57]", hash[:8], string(st.Resume["language"]))
		}
		t.Logf("%s: ✓ %q status=%s searchable=%v langs=%s nextIncomplete=%v", hash[:8], found.Title, found.Status, found.Searchable, string(st.Resume["language"]), st.NextIncomplete)
	}
}

var _ = errors.New // keep the import list honest when probing subsets
var _ = regexp.MustCompile
