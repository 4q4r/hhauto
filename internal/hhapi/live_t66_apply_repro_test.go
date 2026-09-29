//go:build live66

package hhapi

// T66 LIVE REPRODUCTION PROBE (manual, network + owner session required;
// never in the default gate):
//
//	go test -tags live66 -run TestLive66ApplyRepro -count=1 -v ./internal/hhapi/
//
// Replays the failing apply path on clone 943358f3… with full
// instrumentation: plan-era read → live-at-apply read → diffContentKeys →
// per-key POST in prod (sorted) order with sent-body dumps → verify verdict
// per key with the first-mismatch JSON path for failures.
//
// The single content mutation is a marker suffix on the FIRST own
// experience description (mirrors the failing improve plan, whose
// profile.experience edits failed verify); the marker is removed again on
// the next real improve run. The no-op title POST (task-authorized)
// reproduces the "a save happened recently" precondition that flips hh's
// specializationTree translit normalization between reads.

import (
	"context"
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

func TestLive66ApplyRepro(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	dir := "/tmp/opencode/t66"
	_ = os.MkdirAll(dir, 0o700)

	// Plan-era read (what ImproveClone holds while the AI runs).
	plan, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("plan content: %v", err)
	}
	t.Logf("[plan] title=%s spec-sha=%s", plan.Resume["title"], live66Sha(plan.Resume["specializationTree"]))

	// Precondition: one save POST right before the apply read (the
	// failing run's apply followed the keyskill/other saves; the probe
	// uses the authorized no-op title POST).
	if err := c.postResumeEdit(ctx, live66Clone, "title", plan.Resume["title"]); err != nil {
		t.Fatalf("title no-op POST: %v", err)
	}

	// The plan mutation: marker on the first OWN experience record.
	mutated := plan.Clone()
	var exps []map[string]json.RawMessage
	if err := json.Unmarshal(mutated.Profile["experience"], &exps); err != nil {
		t.Fatalf("experience decode: %v", err)
	}
	selfHash := live66Clone
	marked := -1
	for i, it := range exps {
		if !sharedRecordSelf(it, selfHash) {
			continue
		}
		var desc string
		if err := json.Unmarshal(it["description"], &desc); err != nil {
			t.Fatalf("description decode: %v", err)
		}
		desc += " [T66-маркер-воспроизведение]"
		b, _ := json.Marshal(desc)
		it["description"] = b
		marked = i
		break
	}
	if marked < 0 {
		t.Fatalf("no own experience record found; refusing to probe")
	}
	mb, _ := json.Marshal(exps)
	mutated.Profile["experience"] = mb

	// Live-at-apply read + the exact prod diff.
	live2, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("live content: %v", err)
	}
	changed := diffContentKeys(live2, mutated)
	sort.Strings(changed)
	t.Logf("[apply] changed keys (live2 vs payload): %v", changed)

	// POST each changed key in prod order, dumping the sent body.
	for _, key := range changed {
		side, name := splitContentKey(key)
		sentBody, _ := json.Marshal(map[string]json.RawMessage{name: editFlatValue(sideOf(mutated, side)[name])})
		name2 := fmt.Sprintf("%s/sent-%s.json", dir, strings.ReplaceAll(key, ".", "_"))
		_ = os.WriteFile(name2, sentBody, 0o600)
		t.Logf("[apply] POST %s body=%d bytes → %s", key, len(sentBody), name2)
		if verr := c.postResumeEdit(ctx, live66Clone, name, sideOf(mutated, side)[name]); verr != nil {
			t.Fatalf("POST %s: %v", key, verr)
		}
	}

	// Verify pass with per-key verdicts and first-mismatch paths, polling
	// like the prod gate (10s interval) to watch convergence live.
	var missing []string
	for poll := 0; poll < 6; poll++ {
		if poll > 0 {
			time.Sleep(10 * time.Second)
		}
		verify, err := c.ResumeContent(ctx, live66Clone)
		if err != nil {
			t.Fatalf("verify content: %v", err)
		}
		missing = nil
		for _, key := range changed {
			side, name := splitContentKey(key)
			got := sideOf(verify, side)[name]
			want := sideOf(mutated, side)[name]
			verdict := appliedEqual(got, want)
			t.Logf("[verify p%d] %s appliedEqual=%v got-sha=%s want-sha=%s", poll, key, verdict, live66Sha(got), live66Sha(want))
			if !verdict {
				t.Logf("[verify p%d] %s first mismatch: %s", poll, key, live66FirstMismatch(string(want), string(got)))
				_ = os.WriteFile(fmt.Sprintf("%s/got-p%d-%s.json", dir, poll, strings.ReplaceAll(key, ".", "_")), got, 0o600)
				live66Decompose(t, key, got, want)
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			t.Logf("[verify] all keys verified at poll %d", poll)
			break
		}
	}
	for _, key := range missing {
		t.Logf("[verify FAILED] %s", key)
	}
	// Count own records for the invariant note.
	var own int
	for _, it := range exps {
		if sharedRecordSelf(it, selfHash) {
			own++
		}
	}
	t.Logf("[done] own experience records in payload: %d (marker on #%d)", own, marked)
}

// live66FirstMismatch walks two JSON documents and reports the first leaf
// path where they disagree under verify-gate semantics (strings via
// normAppliedText, everything else structurally).
func live66FirstMismatch(want, got string) string {
	var wv, gv any
	if json.Unmarshal([]byte(want), &wv) != nil || json.Unmarshal([]byte(got), &gv) != nil {
		return "decode failure"
	}
	if path, ok := live66DiffPath("", wv, gv); !ok {
		return "none (canonically equal)"
	} else {
		return path
	}
}

func live66DiffPath(path string, w, g any) (string, bool) {
	switch wv := w.(type) {
	case map[string]any:
		gv, ok := g.(map[string]any)
		if !ok {
			return path + " (type)", true
		}
		keys := make([]string, 0, len(wv))
		for k := range wv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			g2, ok := gv[k]
			if !ok {
				return path + "/" + k + " (missing in read-back)", true
			}
			if p, bad := live66DiffPath(path+"/"+k, wv[k], g2); bad {
				return p, true
			}
		}
		return "", false
	case []any:
		gv, ok := g.([]any)
		if !ok {
			return path + " (type)", true
		}
		for i := range wv {
			if i >= len(gv) {
				return fmt.Sprintf("%s[%d] (missing in read-back)", path, i), true
			}
			if p, bad := live66DiffPath(fmt.Sprintf("%s[%d]", path, i), wv[i], gv[i]); bad {
				return p, true
			}
		}
		return "", false
	case string:
		gs, ok := g.(string)
		if !ok {
			return path + " (type)", true
		}
		if normAppliedText(wv) != normAppliedText(gs) {
			return fmt.Sprintf("%s (text) want=%q got=%q", path, truncate66(wv), truncate66(gs)), true
		}
		return "", false
	default:
		if fmt.Sprint(w) != fmt.Sprint(g) {
			return fmt.Sprintf("%s want=%v got=%v", path, w, g), true
		}
		return "", false
	}
}

func truncate66(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// live66Decompose prints the step-by-step appliedEqual evaluation for one
// key (lengths, shape classes, per-sent-record coverage verdicts) so a
// false negative is attributable to an exact code path.
func live66Decompose(t *testing.T, key string, got, want json.RawMessage) {
	t.Helper()
	var av, bv any
	ea := json.Unmarshal(got, &av)
	eb := json.Unmarshal(want, &bv)
	t.Logf("[decompose %s] decode got=%v want=%v", key, ea, eb)
	if ea != nil || eb != nil {
		t.Logf("[decompose %s] got-bytes=%s", key, truncate66(string(got)))
		return
	}
	al, aok := av.([]any)
	bl, bok := bv.([]any)
	t.Logf("[decompose %s] lens got=%d want=%d array-ok=%v/%v", key, len(al), len(bl), aok, bok)
	if !aok || !bok {
		t.Logf("[decompose %s] got-bytes=%s want-bytes=%s", key, truncate66(string(got)), truncate66(string(want)))
		return
	}
	if len(al) != len(bl) {
		t.Logf("[decompose %s] LEN MISMATCH — jsonAppliedEqual returns false before shape checks", key)
	}
	ao, bo := allObjects(al), allObjects(bl)
	t.Logf("[decompose %s] allObjects got=%v want=%v", key, ao, bo)
	if ao && bo {
		for i, so := range bl {
			cov := false
			var reason string
			for _, go2 := range al {
				if objectCovers(go2.(map[string]any), so.(map[string]any)) {
					cov = true
					break
				}
			}
			if !cov {
				reason, _ = live66DiffPath(fmt.Sprintf("sent[%d]", i), so, map[string]any{})
			}
			t.Logf("[decompose %s] sent[%d] covered=%v %s", key, i, cov, reason)
		}
	}
}
