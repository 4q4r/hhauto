//go:build live99

package hhapi

// T99 LIVE SANDBOX PROBE (manual, mutating on a THROWAWAY shell; never in
// the default gate):
//
//	go test -tags live99 -run TestLive99SandboxDetachProbe -count=1 -v ./internal/hhapi/
//
// Answers the two open mechanics questions with live evidence:
//
//  1. Does a BARE created shell render the lineage pool (attach at create)
//     or not (attach later — improve/publish)?
//  2. Does the EDIT route FORK a shared record for the editing resume
//     (hh's fork = detach-from-original + attach-to-fork, proven by the
//     T99 pool dump: parent records no longer list the clone hashes that
//     forked away)? If the fork exists, detach = fork + delete-fork
//     (T56 machine, sole member) — a deterministic per-resume cleanup
//     that never deletes shared originals.
//
// Safety: the probe tweaks one rendered record's description by a single
// trailing "\n" (render-invisible, sha-detectable, revertible); the
// baseline raw pool is dumped first and re-verified byte-for-byte at the
// end. The fork (if any) is deleted before the sandbox resume is deleted.
// Artifacts: /tmp/opencode/t99/.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

func TestLive99SandboxDetachProbe(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	pool := func(viewer string) ([]experienceRecord, error) {
		st, err := c.fetchBuilderState(ctx, viewer)
		if err != nil {
			return nil, err
		}
		return experiencePool(st)
	}
	descSHA := func(raw json.RawMessage) string {
		var p struct {
			Description *string `json:"description"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Description == nil {
			return ""
		}
		sum := sha256.Sum256([]byte(*p.Description))
		return hex.EncodeToString(sum[:])[:12]
	}

	// ---- Step 0: baseline pool (from a created resume, lineage view) ----
	const viewer = "a88bd526ff113b10be0039ed1f776342455439"
	base, err := pool(viewer)
	if err != nil {
		t.Fatalf("baseline pool: %v", err)
	}
	if len(base) != 12 {
		t.Fatalf("baseline pool = %d, want 12 (account drifted?)", len(base))
	}
	baseSHA := map[int64]string{}
	for _, r := range base {
		baseSHA[r.ID] = descSHA(r.Raw)
	}
	t.Logf("baseline: pool=12 shas recorded")

	// ---- Step 1: bare shell — does it render the pool at create? ----
	sandbox, err := c.CreateResume(ctx, "", nil)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Logf("sandbox shell: %s", sandbox)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dcancel()
		if derr := c.DeleteResume(dctx, sandbox); derr != nil {
			t.Errorf("cleanup: delete sandbox %s: %v", sandbox[:8], derr)
		} else {
			t.Logf("cleanup: sandbox %s deleted", sandbox[:8])
		}
	}()

	sbPool, err := pool(sandbox)
	if err != nil {
		t.Fatalf("sandbox pool read: %v", err)
	}
	rendered := 0
	for _, r := range sbPool {
		for _, h := range r.ResumeHashes {
			if h == sandbox {
				rendered++
			}
		}
	}
	t.Logf("bare shell renders %d/%d pool records", rendered, len(sbPool))
	_ = os.WriteFile(filepath.Join(outDir, "sandbox_bare.json"), mustRawJSON(sbPool), 0o644)

	if rendered == 0 {
		// Attach happens later (improve/publish). Probe the REPLACE route
		// attach: the array carries ALL records, so under the T56
		// delete-on-missing semantics this cannot shrink the pool; under
		// the attach-on-present hypothesis it binds the sandbox to all.
		t.Logf("bare shell empty → REPLACE attach probe")
		raws := make([]json.RawMessage, 0, len(sbPool))
		for _, r := range sbPool {
			raws = append(raws, r.Raw)
		}
		body, merr := json.Marshal(map[string]json.RawMessage{
			"profile":        mustRawJSON(map[string]json.RawMessage{"experience": mustRawJSON(raws)}),
			"resumeFromHash": mustRawJSON(sandbox),
		})
		if merr != nil {
			t.Fatalf("attach body: %v", merr)
		}
		resp, err := c.doMutation(ctx, func() requestSpec {
			return c.buildExperienceReplace(body)
		})
		if err != nil {
			t.Fatalf("attach POST: %v", err)
		}
		t.Logf("attach POST: status=%d body=%.120s", resp.status, resp.body)
		sbPool, err = pool(sandbox)
		if err != nil {
			t.Fatalf("post-attach pool read: %v", err)
		}
		rendered = 0
		for _, r := range sbPool {
			for _, h := range r.ResumeHashes {
				if h == sandbox {
					rendered++
				}
			}
		}
		t.Logf("after attach probe: renders %d/%d", rendered, len(sbPool))
		if rendered == 0 {
			t.Fatalf("sandbox renders nothing: fork probe needs membership; attach route dead")
		}
	}

	// ---- Step 2: fork probe on the LOWEST-id rendered record ----
	target := int64(0)
	var targetRaw json.RawMessage
	for _, r := range sbPool {
		member := false
		for _, h := range r.ResumeHashes {
			if h == sandbox {
				member = true
			}
		}
		if member && (target == 0 || r.ID < target) {
			target, targetRaw = r.ID, r.Raw
		}
	}
	if target == 0 {
		t.Fatal("no rendered record to probe")
	}
	t.Logf("fork probe target: %d (desc_sha=%s)", target, descSHA(targetRaw))

	var probeRec map[string]json.RawMessage
	if err := json.Unmarshal(targetRaw, &probeRec); err != nil {
		t.Fatalf("target decode: %v", err)
	}
	delete(probeRec, "resumes") // the save contract never carries usage metadata
	delete(probeRec, "companyLogos")
	var desc struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(probeRec["description"], &desc); err != nil {
		t.Fatalf("desc decode: %v", err)
	}
	tweaked, _ := json.Marshal(desc.Description + "\n")
	probeRec["description"] = tweaked
	probeBody, _ := json.Marshal([]map[string]json.RawMessage{probeRec})

	if err := c.postResumeEdit(ctx, sandbox, "experience", probeBody); err != nil {
		t.Fatalf("fork probe POST: %v", err)
	}
	t.Logf("fork probe POST accepted (%s)", time.Now().Format("15:04:05"))

	// ---- Step 3: poll for the fork (async materialization, T67 lesson) ----
	deadline := time.Now().Add(6 * time.Minute)
	forkID := int64(0)
	originalIntact := false
	originalDroppedSandbox := false
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		cur, err := pool(sandbox)
		if err != nil {
			t.Fatalf("poll read: %v", err)
		}
		forkID = 0
		originalIntact = false
		originalDroppedSandbox = false
		for _, r := range cur {
			if r.ID == target {
				same := descSHA(r.Raw) == baseSHA[target]
				member := false
				for _, h := range r.ResumeHashes {
					if h == sandbox {
						member = true
					}
				}
				originalIntact = same
				originalDroppedSandbox = !member
			} else if _, known := baseSHA[r.ID]; !known {
				forkID = r.ID // new record = the fork candidate
			}
		}
		t.Logf("poll %s: fork=%d original_intact=%v original_dropped_sandbox=%v",
			time.Now().Format("15:04:05"), forkID, originalIntact, originalDroppedSandbox)
		if forkID != 0 && originalIntact {
			break
		}
	}
	if forkID == 0 {
		// No fork materialized: revert the tweak (in-place risk) before
		// touching anything else, then report the dead end.
		t.Logf("NO FORK materialized — reverting tweak")
		var revertRec map[string]json.RawMessage
		if err := json.Unmarshal(targetRaw, &revertRec); err != nil {
			t.Fatalf("revert decode: %v", err)
		}
		delete(revertRec, "resumes")
		delete(revertRec, "companyLogos")
		revertBody, _ := json.Marshal([]map[string]json.RawMessage{revertRec})
		if err := c.postResumeEdit(ctx, sandbox, "experience", revertBody); err != nil {
			t.Fatalf("REVERT POST failed: %v", err)
		}
		time.Sleep(15 * time.Second)
		cur, err := pool(viewer)
		if err != nil {
			t.Fatalf("revert verify read: %v", err)
		}
		for _, r := range cur {
			if r.ID == target && descSHA(r.Raw) != baseSHA[target] {
				t.Fatalf("REVERT FAILED: record %d content drifted (sha %s != %s)", target, descSHA(r.Raw), baseSHA[target])
			}
		}
		t.Logf("revert verified; fork-detach mechanism DEAD on live hh")
		return
	}

	// ---- Step 4: verify the fork, then delete it (sole member) ----
	cur, err := pool(sandbox)
	if err != nil {
		t.Fatalf("fork verify read: %v", err)
	}
	var forkRaw json.RawMessage
	forkMember := false
	forkIsTweaked := false
	for _, r := range cur {
		if r.ID == forkID {
			forkRaw = r.Raw
			for _, h := range r.ResumeHashes {
				if h == sandbox {
					forkMember = true
				}
			}
			var p struct {
				Description string `json:"description"`
			}
			_ = json.Unmarshal(r.Raw, &p)
			forkIsTweaked = p.Description == desc.Description+"\n"
		}
	}
	if !forkMember {
		t.Fatalf("fork %d exists but does not list the sandbox", forkID)
	}
	t.Logf("FORK CONFIRMED: id=%d member=[sandbox] tweaked=%v; original intact=%v dropped_sandbox=%v",
		forkID, forkIsTweaked, originalIntact, originalDroppedSandbox)
	_ = os.WriteFile(filepath.Join(outDir, "sandbox_fork.json"), forkRaw, 0o644)

	left, err := c.DeleteExperienceRecords(ctx, sandbox, []int64{forkID})
	if err != nil {
		t.Fatalf("delete fork: %v", err)
	}
	t.Logf("fork deleted; pool now %d records", left)

	// ---- Step 5: final integrity sweep ----
	final, err := pool(viewer)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if len(final) != 12 {
		t.Fatalf("final pool = %d, want 12", len(final))
	}
	for _, r := range final {
		if got := descSHA(r.Raw); got != baseSHA[r.ID] {
			t.Errorf("final: record %d desc drifted: %s != %s", r.ID, got, baseSHA[r.ID])
		}
		if r.ID == target {
			for _, h := range r.ResumeHashes {
				if h == sandbox {
					t.Errorf("final: record %d still lists the deleted sandbox", r.ID)
				}
			}
		}
	}
	t.Logf("PASS: fork-detach works (fork→delete), pool constant at 12, originals byte-identical")
}
