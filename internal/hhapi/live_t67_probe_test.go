//go:build live67

package hhapi

// T67 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live67 -run TestLive67PoolDump -count=1 -v ./internal/hhapi/
//	go test -tags live67 -run TestLive67ExperienceVerifyProbe -count=1 -v ./internal/hhapi/
//
// TestLive67PoolDump is READ-ONLY: dumps the experience lineage pool of
// both clones (own/parent split, duplicate groups, per-record shas) and
// re-reads each clone twice to catch read-to-read flicker of
// profile.experience without any mutation. Dumps land in /tmp/opencode/t67/.
//
// TestLive67ExperienceVerifyProbe reproduces the failing verify step in
// minimal form on 79d39537… (marker suffix on the FIRST own description,
// the prod POST path, then the verify poll with a per-sent-record coverage
// decompose and the first-mismatch JSON path). The marker is removed by
// the next real improve run (T66 precedent).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const (
	live67Fullstack = "79d39537ff112c2ff30039ed1f733465596835"
	live67Python    = "943358f3ff112c2ff40039ed1f325344524b36"
	live67Marker    = " [T67-маркер-диагностика]"
)

func live67Sha(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:12]
}

func live67Client(t *testing.T) *Client {
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
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, WithBrowserTLSSpec(spec), WithExperienceReplace(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c
}

type live67Rec struct {
	ID          int64  `json:"id"`
	Company     string `json:"companyName"`
	Position    string `json:"position"`
	Description string `json:"description"`
	StartDate   string `json:"startDate"`
	EndDate     string `json:"endDate"`
	Resumes     []struct {
		Hash string `json:"hash"`
	} `json:"resumes"`
}

// live67Group is the T56 duplicate key: company + dates + position.
func live67Group(r live67Rec) string {
	return r.Company + "|" + r.StartDate + "|" + r.EndDate + "|" + r.Position
}

func live67DumpPool(t *testing.T, c *Client, hash, label, dir string, pass int) {
	t.Helper()
	st, err := c.fetchBuilderState(context.Background(), hash)
	if err != nil {
		t.Fatalf("builder state %s: %v", hash, err)
	}
	pool, err := experiencePool(st)
	if err != nil {
		t.Fatalf("pool %s: %v", hash, err)
	}
	resume, profile := splitBuilderResume(st)
	filterSharedRecords(resume, hash)
	filterSharedRecords(profile, hash)
	var recs []live67Rec
	if err := json.Unmarshal(st.ProfileFields["experience"], &recs); err != nil {
		t.Fatalf("recs decode: %v", err)
	}
	var own, parent int
	groups := map[string][]string{} // group key -> record ids
	for _, r := range recs {
		if len(r.Resumes) == 0 || len(r.Resumes) == 1 && r.Resumes[0].Hash == hash {
			own++
			groups[live67Group(r)] = append(groups[live67Group(r)], fmt.Sprint(r.ID))
		} else {
			parent++
		}
	}
	t.Logf("[%s p%d] pool=%d own=%d parent=%d", label, pass, len(pool), own, parent)
	for g, ids := range groups {
		if len(ids) > 1 {
			t.Logf("[%s p%d] DUP GROUP %q ids=%v", label, pass, g, ids)
		}
	}
	for _, r := range recs {
		tag := "parent"
		if len(r.Resumes) == 0 || len(r.Resumes) == 1 && r.Resumes[0].Hash == hash {
			tag = "OWN"
		}
		t.Logf("[%s p%d] %s id=%d %s | %s | %s—%s desc_sha=%s resumes=%d", label, pass, tag, r.ID, r.Company, r.Position, r.StartDate, r.EndDate, live67Sha(json.RawMessage(r.Description)), len(r.Resumes))
	}
	// Raw record dump for the fixture.
	name := fmt.Sprintf("%s/pool-%s-p%d.json", dir, label, pass)
	b, _ := json.MarshalIndent(st.ProfileFields["experience"], "", " ")
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatalf("dump: %v", err)
	}
	// Split-level experience (what the verify gate sees).
	name = fmt.Sprintf("%s/split-%s-p%d.json", dir, label, pass)
	if err := os.WriteFile(name, profile["experience"], 0o600); err != nil {
		t.Fatalf("dump split: %v", err)
	}
	t.Logf("[%s p%d] split exp sha=%s raw exp sha=%s", label, pass, live67Sha(profile["experience"]), live67Sha(st.ProfileFields["experience"]))
}

func TestLive67PoolDump(t *testing.T) {
	c := live67Client(t)
	dir := "/tmp/opencode/t67"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("dir: %v", err)
	}
	for _, tc := range []struct {
		hash, label string
	}{{live67Fullstack, "fullstack"}, {live67Python, "python"}, {"4f48f357ff112c2fcc0039ed1f6c7555414349", "ref"}} {
		live67DumpPool(t, c, tc.hash, tc.label, dir, 1)
	}
	time.Sleep(5 * time.Second)
	// Second read of the fullstack clone: flicker detection on
	// profile.experience between reads without any POST.
	live67DumpPool(t, c, live67Fullstack, "fullstack", dir, 2)
}

// TestLive67RefRegression re-runs the T66 no-op contract on the reference
// clone 4f48f357 through the T67 code: prod no-op apply (no diff → no
// POST), title no-op POST, experience no-op POST, then the pool must stay
// byte-identical (3 own + 9 parent, keeps' desc shas unchanged).
func TestLive67RefRegression(t *testing.T) {
	c := live67Client(t)
	hash := "4f48f357ff112c2fcc0039ed1f6c7555414349"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	before, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	beforeSha := live67Sha(before.Profile["experience"])
	t.Logf("[regress] before: title=%s exp-sha=%s", before.Resume["title"], beforeSha)

	// Prod no-op apply: no diff → nothing posted, nil result.
	if err := c.UpdateResumeContent(ctx, hash, before.Clone(), []string{"resume.title", "profile.experience"}); err != nil {
		t.Fatalf("no-op apply: %v", err)
	}
	// Direct no-op POSTs (the T66 probe contract): title, then experience.
	if err := c.postResumeEdit(ctx, hash, "title", before.Resume["title"]); err != nil {
		t.Fatalf("title no-op POST: %v", err)
	}
	if err := c.postResumeEdit(ctx, hash, "experience", editFlatValue(before.Profile["experience"])); err != nil {
		t.Fatalf("experience no-op POST: %v", err)
	}
	after, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if !appliedEqual(after.Profile["experience"], before.Profile["experience"]) {
		t.Fatalf("experience read-back mismatch after no-op POST")
	}
	afterSha := live67Sha(after.Profile["experience"])
	t.Logf("[regress] after: exp-sha=%s byte-identical=%v", afterSha, afterSha == beforeSha)
	if afterSha != beforeSha {
		t.Fatal("reference experience pool changed on a no-op apply")
	}
	live67DumpPool(t, c, hash, "ref", "/tmp/opencode/t67", 3)
}

// TestLive67ExperienceVerifyProbe reproduces the failing verify step in
// minimal form on 79d39537… (marker suffix on the FIRST own description,
// the prod POST path, then a manual verify poll dumping BOTH the split and
// the raw pool per poll — fork landing time and membership lag become
// visible). The marker is removed by the next real improve run (T66
// precedent).
func TestLive67ExperienceVerifyProbe(t *testing.T) {
	c := live67Client(t)
	hash := live67Fullstack
	dir := "/tmp/opencode/t67"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("dir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Second)
	defer cancel()

	plan, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("plan content: %v", err)
	}
	mutated := plan.Clone()
	var exps []map[string]json.RawMessage
	if err := json.Unmarshal(mutated.Profile["experience"], &exps); err != nil {
		t.Fatalf("experience decode: %v", err)
	}
	marked := -1
	var preIDs []int64
	for i, it := range exps {
		var p struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(it["id"], &p)
		preIDs = append(preIDs, p.ID)
		if i == 0 {
			var desc string
			if err := json.Unmarshal(it["description"], &desc); err != nil {
				t.Fatalf("description decode: %v", err)
			}
			desc += live67Marker
			b, _ := json.Marshal(desc)
			it["description"] = b
			marked = i
		}
	}
	if marked < 0 {
		t.Fatalf("no own experience record found; refusing to probe")
	}
	t.Logf("[probe] pre own ids=%v marking exp[%d]", preIDs, marked)
	mb, _ := json.Marshal(exps)
	mutated.Profile["experience"] = mb
	_ = os.WriteFile(dir+"/sent-experience.json", mutated.Profile["experience"], 0o600)

	// The prod POST for profile.experience, then the manual verify poll
	// with per-poll dumps of BOTH the split (resumes-filtered) and the raw
	// pool — the fork's landing time and membership lag become visible.
	started := time.Now()
	if verr := c.postResumeEdit(ctx, hash, "experience", editFlatValue(mutated.Profile["experience"])); verr != nil {
		t.Fatalf("POST experience: %v", verr)
	}
	t.Logf("[probe] POST experience ok in %s", time.Since(started).Round(time.Millisecond))

	var covered bool
	for poll := 0; poll < 30 && !covered; poll++ {
		if poll > 0 {
			time.Sleep(10 * time.Second)
		}
		elapsed := time.Since(started).Round(time.Second)
		got, rerr := c.ResumeContent(ctx, hash)
		if rerr != nil {
			t.Fatalf("verify read p%d: %v", poll, rerr)
		}
		ok := experienceCovered(got.Profile["experience"], mutated.Profile["experience"])
		// Raw-pool coverage: the same predicate against the unfiltered
		// builder-state experience array.
		st, serr := c.fetchBuilderState(ctx, hash)
		if serr != nil {
			t.Fatalf("raw state p%d: %v", poll, serr)
		}
		rawOK := experienceCovered(st.ProfileFields["experience"], mutated.Profile["experience"])
		// Own membership: which pool records carry selfHash now?
		var pool []live67Rec
		_ = json.Unmarshal(st.ProfileFields["experience"], &pool)
		var ownIDs []int64
		for _, r := range pool {
			for _, rr := range r.Resumes {
				if rr.Hash == hash {
					ownIDs = append(ownIDs, r.ID)
				}
			}
		}
		sort.Slice(ownIDs, func(i, j int) bool { return ownIDs[i] < ownIDs[j] })
		t.Logf("[probe p%d +%s] split-covered=%v raw-covered=%v own=%v", poll, elapsed, ok, rawOK, ownIDs)
		_ = os.WriteFile(fmt.Sprintf("%s/split-p%d.json", dir, poll), got.Profile["experience"], 0o600)
		b, _ := json.Marshal(st.ProfileFields["experience"])
		_ = os.WriteFile(fmt.Sprintf("%s/raw-p%d.json", dir, poll), b, 0o600)
		if ok || rawOK {
			covered = true
			t.Logf("[probe] covered at +%s (split=%v raw=%v)", elapsed, ok, rawOK)
		}
	}
	if !covered {
		t.Logf("[probe] NOT covered within budget — the failure reproduces")
	}
}

func truncate67(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// live67DiffPath finds the first differing leaf between a sent record and
// a read-back record under objectCovers semantics.
func live67DiffPath(path string, sent map[string]any, got map[string]any) (string, bool) {
	keys := make([]string, 0, len(sent))
	for k := range sent {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "id" {
			continue
		}
		gv, ok := got[k]
		if !ok {
			return path + "/" + k + " (missing in read-back)", true
		}
		if p, bad := live67DiffValue(path+"/"+k, sent[k], gv); bad {
			return p, true
		}
	}
	return "", false
}

func live67DiffValue(path string, sent, got any) (string, bool) {
	switch sv := sent.(type) {
	case string:
		gs, ok := got.(string)
		if !ok {
			return path + " (type)", true
		}
		if ns, ng := normAppliedText(sv), normAppliedText(gs); ns != ng && !strings.Contains(ng, ns) {
			return fmt.Sprintf("%s (text) sent=%q got=%q", path, truncate67(sv), truncate67(gs)), true
		}
		return "", false
	case map[string]any:
		gm, ok := got.(map[string]any)
		if !ok {
			return path + " (type)", true
		}
		return live67DiffPath(path, sv, gm)
	case []any:
		gl, ok := got.([]any)
		if !ok {
			return path + " (type)", true
		}
		for i := range sv {
			if i >= len(gl) {
				return fmt.Sprintf("%s[%d] (missing)", path, i), true
			}
			if p, bad := live67DiffValue(fmt.Sprintf("%s[%d]", path, i), sv[i], gl[i]); bad {
				return p, true
			}
		}
		return "", false
	default:
		if fmt.Sprint(sent) != fmt.Sprint(got) {
			return fmt.Sprintf("%s sent=%v got=%v", path, sent, got), true
		}
		return "", false
	}
}
