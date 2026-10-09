//go:build live96

package hhapi

// T96 LIVE CLEANUP PROBE (manual, outside the gate): verifies the shell
// created by the live `resume create --title "Тест T96"` chain in the
// resumes shard, deletes it through the production DeleteResume (BFF host,
// the strict {url} envelope gate) and proves it left the shard. The target
// hash is pinned to the shell THIS task created — the probe refuses
// anything else, so the owner's three base resumes are untouchable.
//
//	go test -tags live96 -run TestLive96Cleanup -count=1 -v ./internal/hhapi/

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

// live96ShellHash is the shell the live create→improve chain produced
// (see /tmp/opencode/t96/live-create-improve.log).
const live96ShellHash = "77ae6180ff113af0350039ed1f304475336c77"

func live96Client(t *testing.T) *Client {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c
}

// live96DumpShard writes the shard snapshot to the t96 dump dir and
// returns the resume rows keyed by hash.
func live96DumpShard(t *testing.T, c *Client, ctx context.Context, name string) map[string]Resume {
	t.Helper()
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("shard read: %v", err)
	}
	byHash := make(map[string]Resume, len(resumes))
	for _, r := range resumes {
		byHash[r.HHID] = r
	}
	dumpDir := "/tmp/opencode/t96"
	if err := os.MkdirAll(dumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	out, _ := json.MarshalIndent(resumes, "", "  ")
	dump := filepath.Join(dumpDir, name)
	if err := os.WriteFile(dump, out, 0o600); err != nil {
		t.Fatalf("dump write: %v", err)
	}
	t.Logf("shard snapshot: %d resumes -> %s", len(resumes), dump)
	return byHash
}

func TestLive96Cleanup(t *testing.T) {
	c := live96Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	before := live96DumpShard(t, c, ctx, "shard-before.json")
	shell, ok := before[live96ShellHash]
	if !ok {
		t.Fatalf("T96 shell %s not in the shard — nothing to clean (already deleted?)", live96ShellHash)
	}
	t.Logf("T96 shell present: title=%q status=%q searchable=%v", shell.Title, shell.Status, shell.Searchable)

	// Guard: the probe deletes ONLY the shell this task created.
	if shell.Status == "modified" && shell.Title != "" && len(before) <= 3 {
		t.Fatalf("refusing: %s does not look like a T96 test shell", live96ShellHash)
	}

	if err := c.DeleteResume(ctx, live96ShellHash); err != nil {
		t.Fatalf("DeleteResume: %v", err)
	}
	t.Log("DeleteResume accepted ({url} envelope gate passed)")

	after := live96DumpShard(t, c, ctx, "shard-after.json")
	if _, still := after[live96ShellHash]; still {
		t.Fatalf("T96 shell %s STILL in the shard after delete", live96ShellHash)
	}
	t.Logf("T96 shell left the shard; shard now holds %d resumes", len(after))
	for h := range after {
		if _, was := before[h]; !was {
			t.Errorf("unexpected new resume %s appeared during cleanup", h)
		}
	}
	t.Log("live96 cleanup: DONE")
}
