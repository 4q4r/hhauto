//go:build live56

package hhapi

// T56 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live56 -run TestLive56ExperienceDeleteProbe -count=1 -v ./internal/hhapi/
//
// Deletes experience records of the LIVE clone HHAUTO_T56_HASH via the
// REAL editor delete route captured live on 2026-09-29 (cloakbrowser UI
// click "Удалить" on /profile/edit/experience/<id>?resumeFrom=<hash>):
//
//	POST https://resume-profile-front.hh.ru/profile/shards/profile/update
//	{"profile":{"experience":[<all remaining records>]},
//	 "resumeFromHash":"<hash>"}
//	+ X-hhtmSource: profile_experience, X-hhtmFrom: resume,
//	  X-hhtmSourceLabel: resume, X-hhtmFromLabel: resume
//
// REPLACE semantics (UI-proven): every record id missing from the array is
// deleted; records present are kept verbatim, including inherited ones.
// Targets default to the five remaining T56 duplicates (2110985588,
// 2110985589 and 2110984923 were already deleted through the real UI).
// After every single-record POST the probe re-reads the state and asserts
// the total dropped by exactly one and every kept description is
// byte-identical to the pre-delete snapshot. A target whose resumes list
// mentions any hash other than the probe clone is refused as inherited.
// Base resumes are never touched: reads only, and every POST is bound to
// resumeFromHash + explicit record ids.
//
// No secrets are printed; env vars are not required (defaults below).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const (
	live56DefaultHash      = "4f48f357ff112c2fcc0039ed1f6c7555414349"
	live56DefaultDelete    = "2110994891,2110991452,2110986675,2110985590,2110984924"
	live56ProfileUpdateURL = "https://resume-profile-front.hh.ru/profile/shards/profile/update"
)

func TestLive56ExperienceDeleteProbe(t *testing.T) {
	hash := os.Getenv("HHAUTO_T56_HASH")
	if hash == "" {
		hash = live56DefaultHash
	}
	delSpec := os.Getenv("HHAUTO_T56_DELETE")
	if delSpec == "" {
		delSpec = live56DefaultDelete
	}
	targets := parseLive56IDs(delSpec)
	if len(targets) == 0 {
		t.Fatal("no delete targets")
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	readItems := func() []live56Item {
		t.Helper()
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("builder state: %v", err)
		}
		items, err := live56Experience(st)
		if err != nil {
			t.Fatalf("experience: %v", err)
		}
		return items
	}

	start := readItems()
	live56LogInventory(t, "start", start)
	kept := live56Keeps(start, targets)

	for _, target := range targets {
		before := readItems()
		idx := live56IndexOf(before, target)
		if idx < 0 {
			t.Logf("target %d already absent, skipping", target)
			continue
		}
		if live56SharedHash(before[idx], hash) {
			t.Fatalf("refusing to delete %d: resumes=%v mentions other resumes (inherited record)", target, before[idx].ResumeHashes)
		}
		remaining := make([]json.RawMessage, 0, len(before)-1)
		for j, it := range before {
			if j != idx {
				remaining = append(remaining, it.Raw)
			}
		}
		body, merr := json.Marshal(map[string]json.RawMessage{
			"profile":        live56Marshal(t, map[string]json.RawMessage{"experience": live56Marshal(t, remaining)}),
			"resumeFromHash": live56Marshal(t, hash),
		})
		if merr != nil {
			t.Fatalf("marshal body: %v", merr)
		}
		url := c.eps.resolveExternal("profile_experience_update", live56ProfileUpdateURL)
		resp, perr := c.postLive56(ctx, url, body)
		if perr != nil {
			t.Fatalf("delete %d: transport verdict %v", target, perr)
		}
		t.Logf("delete %d: HTTP %d body[:300]=%s", target, resp.status, snippet(resp.body))
		if resp.status != http.StatusOK {
			t.Fatalf("delete %d: unexpected status %d body=%s", target, resp.status, snippet(resp.body))
		}
		after := readItems()
		live56LogInventory(t, fmt.Sprintf("after delete %d", target), after)
		if len(after) != len(before)-1 {
			t.Fatalf("delete %d: total %d -> %d, want %d", target, len(before), len(after), len(before)-1)
		}
		if live56IndexOf(after, target) >= 0 {
			t.Fatalf("delete %d: record still present after replace", target)
		}
		live56AssertKeeps(t, kept, after)
	}

	final := readItems()
	live56LogInventory(t, "final", final)
	if len(final) != len(start)-len(targets) {
		t.Fatalf("final total %d, want %d", len(final), len(start)-len(targets))
	}
	live56AssertKeeps(t, kept, final)
}

// TestLive56FinalReadback is the read-only post-condition check: the clone
// keeps exactly 3 own + 3 inherited records with valid status, and a base
// resume still carries its three shared experience records untouched.
func TestLive56FinalReadback(t *testing.T) {
	const baseHash = "fe31e03eff110666c30039ed1f4f5a59327851" // READ ONLY
	hash := os.Getenv("HHAUTO_T56_HASH")
	if hash == "" {
		hash = live56DefaultHash
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		t.Fatalf("clone state: %v", err)
	}
	items, err := live56Experience(st)
	if err != nil {
		t.Fatalf("clone experience: %v", err)
	}
	live56LogInventory(t, "clone final", items)
	t.Logf("clone title=%q status=%q searchable=%t screens=%v", st.Title(), st.Status(), st.IsSearchable, st.Screens)
	if len(items) != 6 {
		t.Fatalf("clone: total=%d, want 6", len(items))
	}
	wantOwn := map[int64]bool{2110986673: true, 2110986674: true, 2110998186: true}
	wantParent := map[int64]bool{2103588721: true, 2103588722: true, 2103588723: true}
	own, parent := 0, 0
	for _, it := range items {
		switch {
		case wantOwn[it.ID]:
			own++
		case wantParent[it.ID]:
			parent++
		default:
			t.Fatalf("clone: unexpected record id=%d", it.ID)
		}
	}
	if own != 3 || parent != 3 {
		t.Fatalf("clone: own=%d parent=%d, want 3+3", own, parent)
	}
	if st.Status() == "" || st.Status() == "not_finished" {
		t.Fatalf("clone status invalid: %q", st.Status())
	}

	// The experience list in get_by_hash is the LINEAGE POOL (live T56):
	// every resume of the lineage sees all pool records. After the dedup
	// the pool must be exactly the 3 kept own records + the 3 shared ones.
	bst, err := c.fetchBuilderState(ctx, baseHash)
	if err != nil {
		t.Fatalf("base state: %v", err)
	}
	bitems, err := live56Experience(bst)
	if err != nil {
		t.Fatalf("base experience: %v", err)
	}
	live56LogInventory(t, "base pool (read-only)", bitems)
	wantPool := map[int64]bool{2110986673: true, 2110986674: true, 2110998186: true, 2103588721: true, 2103588722: true, 2103588723: true}
	if len(bitems) != len(wantPool) {
		t.Fatalf("base pool: total=%d, want %d", len(bitems), len(wantPool))
	}
	for _, it := range bitems {
		if !wantPool[it.ID] {
			t.Fatalf("base pool: unexpected record id=%d", it.ID)
		}
	}
}

// postLive56 fires one profile/shards/profile/update POST with the header
// set captured from the live editor delete flow (browser adds Origin,
// Referer, Sec-Fetch and cookies on its own; XSRF is stamped by doMutation).
func (c *Client) postLive56(ctx context.Context, url string, body []byte) (*response, error) {
	return c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/profile/edit/experience"))
		h.Set("Accept", "application/json")
		h.Set("Origin", c.withPinnedHost(c.eps.site))
		h.Set("Sec-Fetch-Site", "same-site")
		h.Set("Content-Type", "application/json")
		h.Set("X-hhtmSource", "profile_experience")
		h.Set("X-hhtmFrom", "resume")
		h.Set("X-hhtmSourceLabel", "resume")
		h.Set("X-hhtmFromLabel", "resume")
		return requestSpec{
			kind:      kindMutation,
			operation: "profile_experience_update",
			method:    http.MethodPost,
			url:       url,
			header:    h,
			body:      body,
		}
	})
}

// live56Item is one experience record kept as raw bytes plus a projection
// for identity checks.
type live56Item struct {
	Raw          json.RawMessage
	ID           int64
	ResumeHashes []string
	DescLen      int
	DescSHA      string // sha256(description), first 12 hex chars
}

func live56Experience(st *builderState) ([]live56Item, error) {
	raw, ok := st.ProfileFields["experience"]
	if !ok {
		return nil, fmt.Errorf("no profile.fields.experience in state")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("experience decode: %w", err)
	}
	items := make([]live56Item, 0, len(arr))
	for _, r := range arr {
		var p struct {
			ID      int64 `json:"id"`
			Resumes []struct {
				Hash string `json:"hash"`
			} `json:"resumes"`
			Description *string `json:"description"`
		}
		if err := json.Unmarshal(r, &p); err != nil {
			return nil, fmt.Errorf("experience item decode: %w", err)
		}
		it := live56Item{Raw: r, ID: p.ID}
		for _, rr := range p.Resumes {
			it.ResumeHashes = append(it.ResumeHashes, rr.Hash)
		}
		if p.Description != nil {
			it.DescLen = len(*p.Description)
			sum := sha256.Sum256([]byte(*p.Description))
			it.DescSHA = hex.EncodeToString(sum[:])[:12]
		}
		items = append(items, it)
	}
	return items, nil
}

// live56SharedHash reports whether the record's resumes list mentions any
// hash other than the probe clone (an inherited/shared record).
func live56SharedHash(it live56Item, selfHash string) bool {
	for _, h := range it.ResumeHashes {
		if h != selfHash {
			return true
		}
	}
	return false
}

func live56IndexOf(items []live56Item, id int64) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// live56Keeps snapshots the identity of every record that must survive.
func live56Keeps(items []live56Item, targets []int64) map[int64]live56Item {
	keeps := make(map[int64]live56Item, len(items))
	for _, it := range items {
		if !live56IsTarget(targets, it.ID) {
			keeps[it.ID] = it
		}
	}
	return keeps
}

func live56IsTarget(targets []int64, id int64) bool {
	for _, t := range targets {
		if t == id {
			return true
		}
	}
	return false
}

func live56AssertKeeps(t *testing.T, keeps map[int64]live56Item, items []live56Item) {
	t.Helper()
	for _, it := range items {
		k, ok := keeps[it.ID]
		if !ok {
			continue
		}
		if it.DescLen != k.DescLen || it.DescSHA != k.DescSHA {
			t.Fatalf("kept record %d changed: desc_len %d->%d desc_sha %s->%s", it.ID, k.DescLen, it.DescLen, k.DescSHA, it.DescSHA)
		}
	}
}

func live56LogInventory(t *testing.T, label string, items []live56Item) {
	t.Helper()
	t.Logf("%s: total=%d", label, len(items))
	for _, it := range items {
		t.Logf("  id=%d resumes=%v desc_len=%d desc_sha=%s", it.ID, it.ResumeHashes, it.DescLen, it.DescSHA)
	}
}

func parseLive56IDs(s string) []int64 {
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(p, "%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// live56Marshal renders v as JSON, failing the test on a marshal error
// (names are records built in-process; a failure here is a probe bug).
func live56Marshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
