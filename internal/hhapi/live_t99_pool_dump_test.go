//go:build live99

package hhapi

// T99 LIVE DUMP PROBE (manual, read-only; never in the default gate):
//
//	go test -tags live99 -run TestLive99PoolDump -count=1 -v ./internal/hhapi/
//
// Dumps the account resumes shard and the experience POOL as seen from
// several resumes of the lineage (the three T97 created resumes, the
// python clone, the fullstack clone and one base resume). For every
// record: id, employer, profession, end date, the resumes[] membership
// list and a description sha prefix. The point is to answer WHY a freshly
// created resume renders pool generations (membership attach at
// create/publish vs render-by-default) — dumps only, zero mutations.
// Artifacts: /tmp/opencode/t99/.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

func TestLive99PoolDump(t *testing.T) {
	outDir := "/tmp/opencode/t99"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("shard: %v", err)
	}
	shardOut, _ := json.MarshalIndent(resumes, "", " ")
	if werr := os.WriteFile(filepath.Join(outDir, "shard.json"), shardOut, 0o644); werr != nil {
		t.Fatalf("write shard: %v", werr)
	}
	for _, r := range resumes {
		t.Logf("shard: hash=%s status=%s searchable=%v percent=%d",
			r.HHID, r.Status, r.Searchable, r.Percent)
	}

	// The lineage members whose pool view matters for T99: the three
	// T97 created resumes, python/fullstack clones, and the python base.
	targets := []string{
		"38549492ff113b0d0b0039ed1f636362577a45", // created: Специалист по ИБ
		"068e9047ff113b0ec00039ed1f357a626b314e", // created: QA Automation (Python)
		"a88bd526ff113b10be0039ed1f776342455439", // created: Преподаватель Python и ИБ
		"943358f3ff112c2ff40039ed1f325344524b36", // python clone
		"79d39537ff112c2ff30039ed1f733465596835", // fullstack clone
		"c8f0d470ff11044ad40039ed1f4b4d72506548", // python base (read-only)
	}
	all := map[string][]dumpRecord{}
	for _, hash := range targets {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("builder state %s: %v", hash[:8], err)
		}
		pool, err := experiencePool(st)
		if err != nil {
			t.Fatalf("pool %s: %v", hash[:8], err)
		}
		records := make([]dumpRecord, 0, len(pool))
		for _, rec := range pool {
			var meta struct {
				CompanyName    *string `json:"companyName"`
				ProfessionName *string `json:"professionName"`
				EndDate        *string `json:"endDate"`
				Description    *string `json:"description"`
			}
			_ = json.Unmarshal(rec.Raw, &meta)
			dr := dumpRecord{
				ID:           rec.ID,
				ResumeHashes: rec.ResumeHashes,
			}
			if meta.CompanyName != nil {
				dr.Company = *meta.CompanyName
			}
			if meta.ProfessionName != nil {
				dr.Profession = *meta.ProfessionName
			}
			if meta.EndDate != nil {
				dr.End = *meta.EndDate
			}
			if meta.Description != nil {
				dr.DescLen = len(*meta.Description)
				sum := sha256.Sum256([]byte(*meta.Description))
				dr.DescSHA = hex.EncodeToString(sum[:])[:12]
			}
			for _, h := range rec.ResumeHashes {
				if h == hash {
					dr.RendersOnSelf = true
				}
			}
			records = append(records, dr)
		}
		all[hash] = records
		b, _ := json.MarshalIndent(records, "", " ")
		name := fmt.Sprintf("pool_%s.json", hash[:8])
		if werr := os.WriteFile(filepath.Join(outDir, name), b, 0o644); werr != nil {
			t.Fatalf("write %s: %v", name, werr)
		}
		t.Logf("=== %s: pool=%d rendered_on_self=%d", hash[:8], len(records), countRendered(records))
		for _, dr := range records {
			mark := "-"
			if dr.RendersOnSelf {
				mark = "RENDER"
			}
			t.Logf("  %d | %-24s | %-28s | end=%-7s | %s | members=%d | len=%d sha=%s",
				dr.ID, dr.Company, dr.Profession, dr.End, mark, len(dr.ResumeHashes), dr.DescLen, dr.DescSHA)
		}
	}
	// Cross-resume pool identity check (T56: one lineage pool).
	ref, ok := all["943358f3ff112c2ff40039ed1f325344524b36"]
	if !ok {
		t.Fatal("python clone pool missing")
	}
	for _, hash := range targets {
		other := all[hash]
		if len(other) != len(ref) {
			t.Errorf("pool size differs: %s=%d vs python=%d", hash[:8], len(other), len(ref))
		}
	}
}

type dumpRecord struct {
	ID            int64    `json:"id"`
	Company       string   `json:"company"`
	Profession    string   `json:"profession"`
	End           string   `json:"end"`
	ResumeHashes  []string `json:"resumes"`
	DescLen       int      `json:"desc_len"`
	DescSHA       string   `json:"desc_sha"`
	RendersOnSelf bool     `json:"renders_on_dumped_resume,omitempty"`
}

func countRendered(records []dumpRecord) int {
	n := 0
	for _, r := range records {
		if r.RendersOnSelf {
			n++
		}
	}
	return n
}
