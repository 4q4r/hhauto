//go:build live99

package hhapi

// T99 LIVE APPLY (manual, mutating on the owner's account; never in the
// default gate):
//
//	go test -tags live99 -run TestLive99ApplyDedup -count=1 -v ./internal/hhapi/
//	go test -tags live99 -run TestLive99IdempotentRecheck -count=1 -v ./internal/hhapi/
//
// Runs the production DetachDuplicateExperience over the three T97
// created resumes (which render every pool generation since the wizard
// publish attached them to all twelve records), then sweeps ALL NINE
// lineage resumes: the six non-created ones must render exactly the same
// records with byte-identical descriptions as the baseline, the pool must
// stay at twelve records from every view, and the created resumes must
// render exactly the newest generation per employer (Фриланс /
// Колос-Юг / Полимер). The recheck test re-runs the dedup and demands
// zero mutations (live idempotency). Artifacts: /tmp/opencode/t99/.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

var live99Created = []string{
	"38549492ff113b0d0b0039ed1f636362577a45", // Специалист по ИБ
	"068e9047ff113b0ec00039ed1f357a626b314e", // QA Automation Engineer (Python)
	"a88bd526ff113b10be0039ed1f776342455439", // Преподаватель Python и ИБ
}

var live99Others = []string{
	"fe31e03eff110666c30039ed1f4f5a59327851", // база: Middle DevOps
	"c8f0d470ff11044ad40039ed1f4b4d72506548", // база: Python Backend
	"7ea102d2ff1107845b0039ed1f6f484e45476a", // база: Fullstack AI
	"4f48f357ff112c2fcc0039ed1f6c7555414349", // клон: DevOps
	"943358f3ff112c2ff40039ed1f325344524b36", // клон: Python
	"79d39537ff112c2ff30039ed1f733465596835", // клон: Fullstack
}

// live99KeepIDs is the mandated target: the newest generation per
// employer (the fullstack clone's improve, highest record ids).
var live99KeepIDs = map[int64]bool{2111066447: true, 2111066448: true, 2111066449: true}

func live99Client(t *testing.T) (*Client, context.CancelFunc) {
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

// live99View reads one resume's rendered experience: record id →
// {company, desc sha, end date}.
type live99Record struct {
	Company string `json:"company"`
	Desc    string `json:"desc_sha"`
	End     string `json:"end"`
}

func live99View(t *testing.T, c *Client, ctx context.Context, hash string) map[int64]live99Record {
	t.Helper()
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		t.Fatalf("view %s: %v", hash[:8], err)
	}
	pool, err := experiencePool(st)
	if err != nil {
		t.Fatalf("pool %s: %v", hash[:8], err)
	}
	out := map[int64]live99Record{}
	for _, rec := range pool {
		if !rec.rendersOn(hash) {
			continue
		}
		var p struct {
			CompanyName string `json:"companyName"`
			EndDate     string `json:"endDate"`
		}
		_ = json.Unmarshal(rec.Raw, &p)
		out[rec.ID] = live99Record{Company: p.CompanyName, Desc: experienceDescSHA(rec.Raw), End: p.EndDate}
	}
	return out
}

func live99Save(t *testing.T, name string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if werr := os.WriteFile(filepath.Join("/tmp/opencode/t99", name), b, 0o644); werr != nil {
		t.Fatalf("write %s: %v", name, werr)
	}
}

func TestLive99ApplyDedup(t *testing.T) {
	if err := os.MkdirAll("/tmp/opencode/t99", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c, cancel := live99Client(t)
	defer cancel()
	ctx := context.Background()

	// ---- Phase A: baseline over all nine resumes ----
	baseline := map[string]map[int64]live99Record{}
	for _, hash := range append(append([]string{}, live99Created...), live99Others...) {
		baseline[hash] = live99View(t, c, ctx, hash)
	}
	live99Save(t, "apply_baseline.json", baseline)
	for _, hash := range live99Others {
		if n := len(baseline[hash]); n != 3 {
			t.Fatalf("baseline %s renders %d records, want 3 (account drifted?)", hash[:8], n)
		}
	}
	for _, hash := range live99Created {
		if n := len(baseline[hash]); n != 12 {
			t.Logf("baseline %s renders %d records (expected 12 pre-fix)", hash[:8], n)
		}
	}

	// ---- Phase B: dedup each created resume with the production method ----
	for _, hash := range live99Created {
		res, err := c.DetachDuplicateExperience(ctx, hash)
		if err != nil {
			t.Fatalf("dedup %s: %v (state stays consistent; rerun continues)", hash[:8], err)
		}
		live99Save(t, fmt.Sprintf("apply_result_%s.json", hash[:8]), res)
		t.Logf("dedup %s: kept=%v detached=%v", hash[:8], res.Kept, res.Detached)
	}

	// ---- Phase C: final sweep ----
	sweep := map[string]map[int64]live99Record{}
	for _, hash := range append(append([]string{}, live99Created...), live99Others...) {
		sweep[hash] = live99View(t, c, ctx, hash)
	}
	live99Save(t, "apply_sweep.json", sweep)

	for _, hash := range live99Created {
		view := sweep[hash]
		if len(view) != 3 {
			t.Errorf("created %s renders %d records, want 3", hash[:8], len(view))
		}
		for id := range view {
			if !live99KeepIDs[id] {
				t.Errorf("created %s renders %d — not in the keep set", hash[:8], id)
			}
		}
	}
	companies := map[string]bool{}
	for _, hash := range live99Created {
		for _, rec := range sweep[hash] {
			companies[rec.Company] = true
		}
	}
	for _, want := range []string{"Фриланс", "ООО «Колос-Юг»", "ООО «Полимер»"} {
		if !companies[want] {
			t.Errorf("employer %q missing from the created renders", want)
		}
	}
	for _, hash := range live99Others {
		before, after := baseline[hash], sweep[hash]
		if len(before) != len(after) {
			t.Errorf("%s renders %d → %d", hash[:8], len(before), len(after))
		}
		for id, rec := range before {
			got, ok := after[id]
			if !ok {
				t.Errorf("%s lost record %d", hash[:8], id)
				continue
			}
			if got.Desc != rec.Desc {
				t.Errorf("%s record %d description drifted: %s != %s", hash[:8], id, got.Desc, rec.Desc)
			}
		}
	}
	// Pool constant: every view sees exactly twelve records.
	for _, hash := range append(append([]string{}, live99Created...), live99Others...) {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("pool count %s: %v", hash[:8], err)
		}
		pool, err := experiencePool(st)
		if err != nil {
			t.Fatalf("pool count %s: %v", hash[:8], err)
		}
		if len(pool) != 12 {
			t.Errorf("%s sees pool=%d, want 12", hash[:8], len(pool))
		}
	}
	// Shard: nine resumes, none lost.
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("shard: %v", err)
	}
	if len(resumes) != 9 {
		t.Errorf("shard has %d resumes, want 9", len(resumes))
	}
	sort.Slice(resumes, func(i, j int) bool { return resumes[i].HHID < resumes[j].HHID })
	for _, r := range resumes {
		t.Logf("shard: %s status=%s searchable=%v", r.HHID[:8], r.Status, r.Searchable)
	}
	t.Logf("APPLY DONE: three created resumes render 3 unique records each; others byte-identical")
}

// TestLive99IdempotentRecheck re-runs the production dedup on the three
// created resumes and demands zero mutations (the live idempotency proof).
func TestLive99IdempotentRecheck(t *testing.T) {
	c, cancel := live99Client(t)
	defer cancel()
	ctx := context.Background()

	for _, hash := range live99Created {
		before := live99View(t, c, ctx, hash)
		res, err := c.DetachDuplicateExperience(ctx, hash)
		if err != nil {
			t.Fatalf("recheck %s: %v", hash[:8], err)
		}
		after := live99View(t, c, ctx, hash)
		if len(res.Detached) != 0 {
			t.Errorf("recheck %s: detached=%v, want zero mutations", hash[:8], res.Detached)
		}
		if len(after) != len(before) {
			t.Errorf("recheck %s: renders %d → %d", hash[:8], len(before), len(after))
		}
		t.Logf("recheck %s: renders=%d kept=%v (no-op)", hash[:8], len(after), res.Kept)
	}
}
