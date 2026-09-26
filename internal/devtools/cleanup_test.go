//go:build cleanup

// cleanup_test.go implements T31 — one-shot deletion of the 13 test resumes
// left by the T27/T28 live probes from the owner's hh.ru account. The owner
// explicitly approved the cleanup (orchestrator brief, 2026-09-26).
//
// The session rides the owner cookies (HH_COOKIES_FILE, header format
// "k=v; k2=v2") injected into a THROWAWAY chromedp profile scoped to the
// hh.ru domain — the same isolation invariant as autoclone.EditorRunner.
// Cookie values are never logged and never leave the process except into
// the browser jar.
//
// Flow: /applicant/resumes (login/captcha → fail BLOCKED, no retries) →
// inventory through the page's own shard (in-page fetch
// /shards/applicant/resumes) → resolve the targets (10 numeric ids +
// 3 hash prefixes, see t27-report §3 and the T28 clones) to full hashes →
// delete each via real UI clicks (card delete control → confirm dialog) →
// verify: exactly the 3 base resumes remain. HH_CLEANUP_DRY=1 lists the
// inventory and dumps card HTML without clicking anything.
//
// Run: go test -tags cleanup -run TestCleanupTestResumes -v

package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const cleanupBaseURL = "https://hh.ru"

// cleanupShardsFetch is the in-page request the resumes page's own SPA
// issues: the shard answers 403 without the XSRF header (axios interceptors
// add X-Xsrftoken from the _xsrf cookie, t27-report §2.3).
const cleanupShardsFetch = `(() => {
  const m = document.cookie.match(/(?:^|;\s*)_xsrf=([^;]+)/);
  const xsrf = m ? decodeURIComponent(m[1]) : '';
  return fetch('/shards/applicant/resumes', {credentials: 'include', headers: {
    'Accept': 'application/json, text/javascript, */*; q=0.01',
    'X-Requested-With': 'XMLHttpRequest',
    'X-Xsrftoken': xsrf
  }}).then(r => r.text().then(b => JSON.stringify({status: r.status, url: location.href, body: b})));
})()`

// cleanupTargetIDs — numeric ids of the not_finished shells (t27-report §3)
// plus the T27 target clone 287869147.
var cleanupTargetIDs = []string{
	"287882681",
	"287883905", "287883906", "287883908", "287883932",
	"287883934", "287883935", "287883936", "287883937",
	"287869147",
}

// cleanupTargetHashes — hash prefixes without a known numeric id: the
// id-less T27 shell and the two T28 full clones («(копия 1)»/«(копия 2)»).
var cleanupTargetHashes = []string{"17a45d5f", "9b61d368ff", "96a2f84cff"}

// cleanupKeepHashes — hash prefixes of the 3 base resumes that must survive.
var cleanupKeepHashes = []string{"7ea102d2", "c8f0d470", "fe31e03e"}

// cleanupResume is one inventory entry resolved from the shard response.
type cleanupResume struct {
	ID     string
	Hash   string
	Title  string
	Status string
}

func TestCleanupTestResumes(t *testing.T) {
	for _, keep := range cleanupKeepHashes {
		for _, target := range cleanupTargetHashes {
			if strings.HasPrefix(keep, target) || strings.HasPrefix(target, keep) {
				t.Fatalf("prefix collision between keep %q and target %q — refusing to run", keep, target)
			}
		}
	}

	cookies := cleanupLoadCookies(t)
	ctx := cleanupStartBrowser(t, cookies)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()

	// 1. Authenticated resumes page; any login/captcha edge → BLOCKED.
	cleanupOpenResumes(t, ctx)

	// 2. Inventory through the page's own shard.
	inv := cleanupInventory(t, ctx)
	dry := os.Getenv("HH_CLEANUP_DRY") != ""
	if dry {
		cleanupDumpForExploration(t, ctx, inv)
		t.Skip("HH_CLEANUP_DRY=1 — inventory only, nothing deleted")
	}

	// 3. Resolve exactly the listed targets to full hashes.
	targets := cleanupSelectTargets(t, inv)
	if len(targets) == 0 {
		t.Logf("none of the 13 targets present — nothing to delete (already cleaned?)")
	}

	// 4. Delete each target through the real UI. Fail-fast: a mid-run
	// failure means the click model is off — stop instead of spraying
	// unconfirmed clicks at hh (budget discipline).
	deleted := 0
	for _, target := range targets {
		if err := cleanupDeleteOne(t, ctx, target.Hash); err != nil {
			t.Fatalf("delete %s (id=%s title=%q): %v — stopping: no blind retries",
				cleanupShortHash(target.Hash), target.ID, target.Title, err)
		}
		deleted++
		t.Logf("deleted: id=%s hash=%s… title=%q", target.ID, cleanupShortHash(target.Hash), target.Title)
	}

	// 5. Authoritative verification: exactly the 3 base resumes remain.
	inv = cleanupInventory(t, ctx)
	if len(inv) != len(cleanupKeepHashes) {
		t.Errorf("after cleanup: %d resumes remain, want %d: %s", len(inv), len(cleanupKeepHashes), cleanupFormat(inv))
	}
	for _, r := range inv {
		if !cleanupHasPrefix(r.Hash, cleanupKeepHashes) {
			t.Errorf("unexpected resume survived: id=%s hash=%s… title=%q", r.ID, cleanupShortHash(r.Hash), r.Title)
		}
	}
	t.Logf("done: deleted=%d remaining=%d %s", deleted, len(inv), cleanupFormat(inv))
}

// --- inventory ----------------------------------------------------------------

// cleanupInventory fetches /shards/applicant/resumes from the page context
// and resolves every hash-bearing entry.
func cleanupInventory(t *testing.T, ctx context.Context) []cleanupResume {
	t.Helper()
	var wrap struct {
		Status int    `json:"status"`
		URL    string `json:"url"`
		Body   string `json:"body"`
	}
	if err := cleanupEvalAwait(ctx, cleanupShardsFetch, &wrap); err != nil {
		t.Fatalf("in-page fetch /shards/applicant/resumes: %v", err)
	}
	if !cleanupIsHH(wrap.URL) {
		t.Fatalf("page context left hh.ru (now %s) — refusing to interpret the response", wrap.URL)
	}
	if wrap.Status != 200 {
		t.Fatalf("/shards/applicant/resumes returned HTTP %d (page %s): %.300s", wrap.Status, wrap.URL, wrap.Body)
	}
	inv := cleanupParseInventory(wrap.Body)
	if inv == nil {
		t.Fatalf("shard response did not parse into resumes: %.500s", wrap.Body)
	}
	return inv
}

// cleanupParseInventory walks any JSON shape and collects hash-bearing
// objects (the shard nests resumes under a wrapper; drafts and published
// resumes live in the same list).
func cleanupParseInventory(raw string) []cleanupResume {
	var root any
	if json.Unmarshal([]byte(raw), &root) != nil {
		return nil
	}
	var out []cleanupResume
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case []any:
			for _, c := range v {
				walk(c)
			}
		case map[string]any:
			hash, _ := v["hash"].(string)
			if hash == "" {
				for _, c := range v {
					walk(c)
				}
				return
			}
			e := cleanupResume{Hash: hash}
			switch id := v["id"].(type) {
			case string:
				e.ID = id
			case float64:
				e.ID = strconv.FormatInt(int64(id), 10)
			}
			e.Title, _ = v["title"].(string)
			if attrs, ok := v["_attributes"].(map[string]any); ok {
				e.Status, _ = attrs["status"].(string)
			}
			if e.Status == "" {
				e.Status, _ = v["status"].(string)
			}
			out = append(out, e)
		}
	}
	walk(root)
	if len(out) == 0 {
		return nil
	}
	return out
}

// cleanupSelectTargets matches the inventory against exactly the listed
// ids and hash prefixes; anything else (the 3 base resumes included) is
// untouchable by construction.
func cleanupSelectTargets(t *testing.T, inv []cleanupResume) []cleanupResume {
	t.Helper()
	var targets []cleanupResume
	seen := map[string]bool{}
	for _, r := range inv {
		if cleanupHasPrefix(r.Hash, cleanupKeepHashes) {
			continue
		}
		hit := cleanupContains(cleanupTargetIDs, r.ID) || cleanupHasPrefix(r.Hash, cleanupTargetHashes)
		if !hit || seen[r.Hash] {
			continue
		}
		seen[r.Hash] = true
		targets = append(targets, r)
	}
	present := cleanupFormat(targets)
	t.Logf("targets resolved: %d of 13 listed; inventory: %d resumes%s",
		len(targets), len(inv), present)
	// The listed 13 minus what is already gone must equal the resolved set.
	alreadyGone := len(cleanupTargetIDs) + len(cleanupTargetHashes) - len(targets)
	t.Logf("already absent (id/prefix not found in inventory): %d", alreadyGone)
	if len(targets) > len(cleanupTargetIDs)+len(cleanupTargetHashes) {
		t.Fatalf("resolved %d targets — more than the 13 listed; aborting", len(targets))
	}
	return targets
}

// --- deletion (real UI clicks) --------------------------------------------------

// cleanupDeleteOne deletes one resume by full hash through the real UI.
// Live-verified shapes (2026-09-26): a draft card carries a direct «Удалить»
// control, while a published resume's ••• menu (data-qa
// resume-list-action-more) has NO delete item — its trash lives on the
// resume page (data-qa=resume-delete). One sequence covers both shapes:
// card control → scoped confirm clicks → shard check → resume-page fallback
// → poll until the hash disappears from the shard. Every step is logged.
func cleanupDeleteOne(t *testing.T, ctx context.Context, hash string) error {
	short := cleanupShortHash(hash)
	var stepRaw, dialogsRaw string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(cleanupClickCardDeleteJS(hash), &stepRaw)); err != nil {
		return fmt.Errorf("card delete click: %w", err)
	}
	var step struct {
		Clicked string `json:"clicked"`
		Kind    string `json:"kind"`
		By      string `json:"by"`
		HTML    string `json:"html"`
	}
	if err := json.Unmarshal([]byte(stepRaw), &step); err != nil {
		return fmt.Errorf("decode card click state: %v (%.200s)", err, stepRaw)
	}
	t.Logf("[%s] card click: clicked=%q kind=%q by=%q html=%.250s",
		short, step.Clicked, step.Kind, step.By, step.HTML)
	if step.Clicked == "no-card" {
		return fmt.Errorf("card with /resume/%s not found in DOM", short)
	}
	if step.Clicked == "no-control" {
		return fmt.Errorf("delete control not found in card; card HTML: %.800s", step.HTML)
	}
	if step.Clicked != "control" {
		return fmt.Errorf("unexpected card click state %q", step.Clicked)
	}
	if err := cleanupSleep(ctx, 900*time.Millisecond); err != nil {
		return err
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(cleanupDialogsDumpJS, &dialogsRaw)); err != nil {
		return fmt.Errorf("dialogs dump: %w", err)
	}
	t.Logf("[%s] dialogs after card click: %s", short, dialogsRaw)

	// 2. Scoped confirm attempts (a modal or the dropdown «Удалить» item).
	if err := cleanupConfirmLoop(t, ctx, short, 2); err != nil {
		return err
	}
	// 3. Already gone (direct deletion, e.g. a draft)?
	gone, err := cleanupShardGone(ctx, hash)
	if err != nil {
		return err
	}
	if gone {
		return nil
	}
	// 4. Resume page fallback: the ••• menu has no «Удалить» for published
	// resumes — the trash button (data-qa=resume-delete) lives on /resume/<hash>.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(cleanupBaseURL+"/resume/"+hash),
		chromedp.Sleep(2500*time.Millisecond)); err != nil {
		return fmt.Errorf("navigate /resume/%s: %w", short, err)
	}
	var resRaw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(cleanupClickResumeDeleteJS, &resRaw)); err != nil {
		return fmt.Errorf("resume-delete click: %w", err)
	}
	var res struct {
		Clicked string `json:"clicked"`
		HTML    string `json:"html"`
	}
	if err := json.Unmarshal([]byte(resRaw), &res); err != nil {
		return fmt.Errorf("decode resume-delete state: %v (%.200s)", err, resRaw)
	}
	t.Logf("[%s] resume page delete: clicked=%q html=%.200s", short, res.Clicked, res.HTML)
	if res.Clicked != "delete" {
		return fmt.Errorf("resume-delete control not found/clickable on /resume/%s", short)
	}
	if err := cleanupSleep(ctx, 900*time.Millisecond); err != nil {
		return err
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(cleanupDialogsDumpJS, &dialogsRaw)); err != nil {
		return fmt.Errorf("dialogs dump: %w", err)
	}
	t.Logf("[%s] dialogs after resume-delete: %s", short, dialogsRaw)
	if err := cleanupConfirmLoop(t, ctx, short, 3); err != nil {
		return err
	}
	// 5. Poll the shard until the hash is gone (authoritative).
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for deletion: %w", err)
		}
		gone, err := cleanupShardGone(ctx, hash)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}
		if time.Now().After(deadline) {
			var where string
			_ = chromedp.Run(ctx, chromedp.Evaluate(`location.href`, &where))
			return fmt.Errorf("hash %s… still present after confirm clicks (page %s)", short, where)
		}
		if err := cleanupSleep(ctx, 1500*time.Millisecond); err != nil {
			return err
		}
	}
}

// cleanupConfirmLoop clicks scoped "Удалить" confirm controls up to n times
// (dropdown item, then dialog button; a "none" result ends the loop).
func cleanupConfirmLoop(t *testing.T, ctx context.Context, short string, n int) error {
	t.Helper()
	for i := 0; i < n; i++ {
		var confRaw string
		if err := chromedp.Run(ctx, chromedp.Evaluate(cleanupClickConfirmJS, &confRaw)); err != nil {
			return fmt.Errorf("confirm click: %w", err)
		}
		var conf struct {
			Clicked string `json:"clicked"`
			Scope   string `json:"scope"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal([]byte(confRaw), &conf); err != nil {
			return fmt.Errorf("decode confirm state: %v (%.200s)", err, confRaw)
		}
		t.Logf("[%s] confirm #%d: clicked=%q scope=%q text=%q", short, i+1, conf.Clicked, conf.Scope, conf.Text)
		if conf.Clicked != "confirm" {
			return nil
		}
		if err := cleanupSleep(ctx, 900*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// cleanupShardGone reports whether the hash has disappeared from the
// account's shard inventory (one in-page fetch; non-200 is an anomaly).
func cleanupShardGone(ctx context.Context, hash string) (bool, error) {
	var wrap struct {
		Status int    `json:"status"`
		URL    string `json:"url"`
		Body   string `json:"body"`
	}
	if err := cleanupEvalAwait(ctx, cleanupShardsFetch, &wrap); err != nil {
		return false, fmt.Errorf("shard fetch: %w", err)
	}
	if !cleanupIsHH(wrap.URL) {
		return false, fmt.Errorf("page context left hh.ru (now %s)", wrap.URL)
	}
	if wrap.Status != 200 {
		return false, fmt.Errorf("shard returned HTTP %d: %.200s", wrap.Status, wrap.Body)
	}
	return !strings.Contains(wrap.Body, hash), nil
}

// cleanupClickCardDeleteJS finds the resume card by its /resume/<hash> link,
// climbs to the card scope and clicks the first delete-like control inside
// it (data-qa / aria-label / title / class / text signatures).
func cleanupClickCardDeleteJS(hash string) string {
	return `(() => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const vis = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const want = ` + strconv.Quote(hash) + `;
  const link = [...document.querySelectorAll('a[href]')].find(a => {
    try { return decodeURIComponent(a.getAttribute('href')).includes(want); } catch (e) { return false; }
  });
  if (!link) return JSON.stringify({clicked: 'no-card'});
  let card = link;
  while (card.parentElement && card !== document.body && !card.querySelector('button,[role="button"]')) {
    card = card.parentElement;
  }
  if (card === document.body) return JSON.stringify({clicked: 'no-control', html: link.outerHTML.slice(0, 800)});
  // Pass 1: explicit delete-like controls; pass 2: the actions/menu (•••)
  // button that reveals the delete item in a portal — strict first, never a
  // blind menu click when a direct delete control exists.
  const sig = el => [el.dataset.qa, el.getAttribute('aria-label'), el.getAttribute('title'),
    String(el.className), norm(el.textContent)].join(' ').toLowerCase();
  const ctrls = [...card.querySelectorAll('button,[role="button"],a,[data-qa]')].filter(vis);
  let del = ctrls.find(el => /delete|remove|trash|удали|корзин/.test(sig(el)));
  let kind = 'delete';
  if (!del) { del = ctrls.find(el => /menu|more|dots|actions|еще|ещё|действи/.test(sig(el))); kind = 'menu'; }
  if (!del) return JSON.stringify({clicked: 'no-control', html: card.outerHTML.slice(0, 2000)});
  const target = del.closest('button,[role="button"],a') || del;
  target.click();
  return JSON.stringify({clicked: 'control', kind, by: (target.dataset.qa || target.getAttribute('aria-label') ||
    norm(target.textContent) || 'control').slice(0, 80), html: target.outerHTML.slice(0, 250)});
})()`
}

// cleanupDialogsDumpJS reports the visible dialog/modal layers (text +
// candidate buttons) — the confirmation evidence for the run log.
const cleanupDialogsDumpJS = `(() => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  const vis = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const sel = '[role="dialog"],[class*="modal"],[class*="Modal"],[class*="popup"],[class*="Popup"],[data-qa*="modal"],[data-qa*="popup"],[data-qa*="confirm"]';
  const ds = [...document.querySelectorAll(sel)].filter(vis).slice(0, 4);
  return JSON.stringify(ds.map(d => ({
    cls: String(d.className).slice(0, 120),
    text: norm(d.innerText).slice(0, 250),
    buttons: [...d.querySelectorAll('button,[role="button"],a')].map(b => norm(b.textContent)).filter(Boolean).slice(0, 10)
  })));
})()`

// cleanupClickConfirmJS clicks the first visible out-of-card clickable whose
// text is a delete action (dropdown item or dialog button; cards excluded —
// the confirm layer always renders in a portal/menu).
// cleanupClickConfirmJS clicks the first "Удалить" confirm control INSIDE a
// visible dialog/menu scope (magritte modal/drop containers) — real controls
// only. Document-wide matching is deliberately avoided: bare wrappers and
// the cookie banner polluted it once (T31 fix round live evidence).
const cleanupClickConfirmJS = `(() => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const vis = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const grab = root => [...root.querySelectorAll('button,[role="button"],[role="menuitem"],a')].filter(el => {
    if (!vis(el) || el.closest('a[href*="/resume/"]')) return false;
    return norm(el.textContent).includes('удалит');
  });
  const scopes = [...document.querySelectorAll('[role="dialog"],[class*="modal"],[class*="Modal"],[class*="drop"],[class*="Drop"]')].filter(vis);
  for (const s of scopes) {
    const hit = grab(s)[0];
    if (hit) {
      hit.click();
      return JSON.stringify({clicked: 'confirm', scope: String(s.className).slice(0, 60),
        text: norm(hit.textContent).slice(0, 60)});
    }
  }
  return JSON.stringify({clicked: 'none'});
})()`

// cleanupClickResumeDeleteJS clicks the trash button (data-qa=resume-delete)
// on the owner view of one resume — the live-verified delete path for
// published resumes whose ••• menu carries no delete item.
const cleanupClickResumeDeleteJS = `(() => {
  const vis = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const btn = document.querySelector('[data-qa="resume-delete"]');
  if (!btn || !vis(btn)) return JSON.stringify({clicked: 'none', html: (btn ? 'not visible' : 'absent')});
  btn.click();
  return JSON.stringify({clicked: 'delete'});
})()`

// cleanupDismissCookieBannerJS closes the "используем cookies" notice once
// so its wrapper stops polluting later click matching (hygiene, not a
// resume action).
const cleanupDismissCookieBannerJS = `(() => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const btn = [...document.querySelectorAll('button,[role="button"]')].find(b => norm(b.textContent) === 'понятно');
  if (!btn) return 'absent';
  btn.click();
  return 'dismissed';
})()`

// --- exploration dump (HH_CLEANUP_DRY=1) ----------------------------------------

func cleanupDumpForExploration(t *testing.T, ctx context.Context, inv []cleanupResume) {
	t.Helper()
	t.Logf("inventory (%d resumes): %s", len(inv), cleanupFormat(inv))
	var text string
	_ = chromedp.Run(ctx, chromedp.Evaluate(
		`document.body.innerText.slice(0, 1500)`, &text))
	t.Logf("page text head: %q", text)
	// Dump up to three cards (one per distinct shape) for selector work.
	var cards string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
  const seen = new Set(); const out = [];
  for (const a of document.querySelectorAll('a[href*="/resume/"]')) {
    let n = a;
    while (n.parentElement && n !== document.body && !n.querySelector('button,[role="button"]')) n = n.parentElement;
    if (n === document.body || seen.has(n.className)) continue;
    seen.add(n.className);
    out.push(n.outerHTML.slice(0, 3000));
    if (out.length >= 3) break;
  }
  return out.join('\n=====\n');
})()`, &cards))
	t.Logf("card dumps:\n%s", cards)
}

// --- page open / BLOCKED gate ---------------------------------------------------

// cleanupOpenResumes navigates to the resumes page and fails BLOCKED on any
// login lander, redirect or challenge (no retries against hh). hh serves the
// anonymous login LANDER at /applicant/resumes itself (no redirect — live
// verified 2026-09-26), so the gate checks positive auth markers, not the URL.
func cleanupOpenResumes(t *testing.T, ctx context.Context) {
	t.Helper()
	var state, raw string
	err := chromedp.Run(ctx,
		// Desktop layout: hh serves the mobile UI (root-mobile-- classes) at
		// the default 800x600 headless viewport, where the delete controls
		// differ — force desktop metrics before the first navigation
		// (T31 fix round, 2026-09-26).
		chromedp.EmulateViewport(1440, 900),
		chromedp.Navigate(cleanupBaseURL+"/applicant/resumes"),
		chromedp.Poll(`(() => {
  const t = (document.body ? document.body.innerText : '').replace(/\u00a0/g, ' ');
  if (document.querySelectorAll('a[href*="/resume/"]').length > 0 || t.includes('Мои резюме')) return 'ready';
  if (t.includes('Войти с паролем') || /\/account\/login|captcha|ddos/i.test(location.href + ' ' + t)) return 'login';
  return '';
})()`, &state, chromedp.WithPollingTimeout(20*time.Second)),
		chromedp.Evaluate(`JSON.stringify({url: location.href,
  text: (document.body ? document.body.innerText : '').slice(0, 600)})`, &raw),
	)
	if err != nil {
		t.Fatalf("navigate %s/applicant/resumes: %v", cleanupBaseURL, err)
	}
	var page struct {
		URL  string `json:"url"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatalf("decode page state: %v (%.200s)", err, raw)
	}
	if state != "ready" {
		t.Fatalf("BLOCKED: not authenticated on %s (state=%q) — session cookies are invalid or expired for browser use; page text head: %q",
			page.URL, state, page.Text)
	}
	var dismissed string
	_ = chromedp.Run(ctx, chromedp.Evaluate(cleanupDismissCookieBannerJS, &dismissed))
	t.Logf("opened %s (authenticated, cookie banner: %s)", page.URL, dismissed)
}

// --- browser / cookies (isolation invariant: throwaway profile, .hh.ru only) ----

// cleanupLoadCookies parses the header-format cookie file. Values are never
// logged.
func cleanupLoadCookies(t *testing.T) []*http.Cookie {
	t.Helper()
	path := os.Getenv("HH_COOKIES_FILE")
	if path == "" {
		path = "/tmp/opencode/hh-cookies.txt"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cookie file: %v", err)
	}
	var cookies []*http.Cookie
	for _, part := range strings.Split(strings.TrimSpace(string(raw)), ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: kv[0], Value: kv[1]})
	}
	if len(cookies) == 0 {
		t.Fatalf("no cookies parsed from %s", path)
	}
	t.Logf("loaded %d cookies from %s", len(cookies), path)
	return cookies
}

// cleanupStartBrowser launches chromedp with a throwaway profile and injects
// the session cookies scoped to .hh.ru before any navigation.
func cleanupStartBrowser(t *testing.T, cookies []*http.Cookie) context.Context {
	t.Helper()
	chromePath, err := cleanupFindChrome()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "hhauto-cleanup-")
	if err != nil {
		t.Fatalf("temp profile: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(dir),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(func() { cancelBrowser(); cancelAlloc() })
	inject := chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range cookies {
			p := network.SetCookie(c.Name, c.Value).
				WithDomain(".hh.ru").WithPath("/").WithSecure(true)
			if err := p.Do(ctx); err != nil {
				return fmt.Errorf("set cookie %s: %w", c.Name, err)
			}
		}
		return nil
	})
	if err := chromedp.Run(browserCtx, inject); err != nil {
		t.Fatalf("inject session cookies: %v", err)
	}
	return browserCtx
}

// cleanupFindChrome mirrors testsolve.FindChrome / findEditorChrome.
func cleanupFindChrome() (string, error) {
	if p := strings.TrimSpace(os.Getenv("HHAUTO_CHROME_PATH")); p != "" {
		if path, err := exec.LookPath(p); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("HHAUTO_CHROME_PATH=%q не существует или не исполняем", p)
	}
	for _, name := range []string{"chrome", "chromium", "google-chrome", "google-chrome-stable", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Chrome не найден (укажите HHAUTO_CHROME_PATH)")
}

// cleanupEvalAwait evaluates an async expression on the page and awaits its
// promise (chromedp.Evaluate has no awaitPromise option in v0.16). The
// expression must return a JSON string (JSON.stringify), decoded into res.
// The work runs through chromedp.Run/ActionFunc so the evaluate rides the
// browser session attached to ctx.
func cleanupEvalAwait(ctx context.Context, expr string, res any) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(pc context.Context) error {
		v, exp, err := runtime.Evaluate(expr).
			WithAwaitPromise(true).WithReturnByValue(true).Do(pc)
		if err != nil {
			return err
		}
		if exp != nil {
			return exp
		}
		var raw string
		if err := json.Unmarshal(v.Value, &raw); err != nil {
			return fmt.Errorf("await result is not a JSON string: %w", err)
		}
		return json.Unmarshal([]byte(raw), res)
	}))
}

// cleanupIsHH reports whether the URL host is hh.ru or a regional *.hh.ru
// host (the resumes page 302s to the account's region, e.g. taganrog.hh.ru).
func cleanupIsHH(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	return host == "hh.ru" || strings.HasSuffix(host, ".hh.ru")
}

func cleanupSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func cleanupHasPrefix(hash string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(hash, p) {
			return true
		}
	}
	return false
}

func cleanupContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func cleanupShortHash(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

func cleanupFormat(inv []cleanupResume) string {
	var b strings.Builder
	for i, r := range inv {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "{id=%s hash=%s… title=%q status=%s}", r.ID, cleanupShortHash(r.Hash), r.Title, r.Status)
	}
	return b.String()
}
