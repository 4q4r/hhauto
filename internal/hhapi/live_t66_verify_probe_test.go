//go:build live66

package hhapi

// T66 LIVE DIAGNOSIS PROBE (manual, network + owner session required; never
// in the default gate):
//
//	go test -tags live66 -run TestLive66VerifyDiagnosis -count=1 -v ./internal/hhapi/
//
// Diagnoses the verify regression on clone 943358f3… (symptom: "resume
// update verify failed for [profile.experience resume.specializationTree]"):
//
//	Phase A — two read-only builder-state reads (~15s apart): full raw dump
//	          to /tmp/opencode/t66/ + phantom-diff census between reads,
//	          including the exact diffContentKeys the apply step would see
//	          for an unchanged plan payload.
//	Phase B — single no-op title POST (current value, the task-authorized
//	          mutation) + raw response dump + re-read diff: does a title
//	          save make hh mutate other sections server-side?
//	Phase R — read-only census of the reference clone 4f48f357….
//
// Base resumes are never touched.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const (
	live66Clone = "943358f3ff112c2ff40039ed1f325344524b36"
	live66Ref   = "4f48f357ff112c2fcc0039ed1f6c7555414349"
)

func TestLive66VerifyDiagnosis(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	dir := "/tmp/opencode/t66"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// rawState fetches the unfiltered builder state and dumps it to dir.
	rawState := func(hash, tag string) map[string]json.RawMessage {
		resp, err := c.do(ctx, func() requestSpec {
			h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
			return requestSpec{
				kind:      kindGet,
				operation: "resume_builder_state",
				method:    http.MethodGet,
				url:       c.endpointURL(epResumeGetByHash, c.eps.siteDefault("/profile/shards/resume/get_by_hash")) + "?resumeHash=" + url.QueryEscape(hash),
				header:    h,
			}
		}())
		if err != nil {
			t.Fatalf("[%s] raw state: %v", tag, err)
		}
		name := filepath.Join(dir, fmt.Sprintf("%s-%s.json", tag, hash[:8]))
		if err := os.WriteFile(name, resp.body, 0o600); err != nil {
			t.Fatalf("dump: %v", err)
		}
		var env struct {
			Resume  map[string]json.RawMessage `json:"resume"`
			Profile struct {
				Fields map[string]json.RawMessage `json:"fields"`
			} `json:"profile"`
		}
		if err := json.Unmarshal(resp.body, &env); err != nil {
			t.Fatalf("[%s] decode: %v", tag, err)
		}
		t.Logf("[%s] raw dump=%s bytes=%d sha=%s", tag, name, len(resp.body), live66Sha(resp.body))
		return env.Resume
	}

	// census logs every resume-side key whose raw value differs + the
	// filtered-split diff for the two symptom sections.
	census := func(tag string, r1, r2 map[string]json.RawMessage, c1, c2 *ResumeContent) {
		var moved []string
		for k, v1 := range r1 {
			if v2, ok := r2[k]; !ok || !rawEqual(v1, v2) {
				moved = append(moved, "resume."+k)
			}
		}
		for k := range r2 {
			if _, ok := r1[k]; !ok {
				moved = append(moved, "resume."+k+"(new)")
			}
		}
		sort.Strings(moved)
		t.Logf("[%s] raw moved keys: %v", tag, moved)
		for _, key := range []string{"profile.experience", "resume.specializationTree"} {
			side, name := splitContentKey(key)
			g1, g2 := sideOf(c1, side)[name], sideOf(c2, side)[name]
			status := "EQUAL"
			if !rawEqual(g1, g2) {
				status = "DIFF"
			}
			t.Logf("[%s] %s filtered: %s len1=%d len2=%d sha1=%s sha2=%s",
				tag, key, status, len(g1), len(g2), live66Sha(g1), live66Sha(g2))
		}
		phantom := diffContentKeys(c2, c1) // live2 vs unchanged plan payload
		sort.Strings(phantom)
		t.Logf("[%s] phantom diffContentKeys(live2, plan1): %v", tag, phantom)
	}

	// ---- Phase R: reference clone, read-only ----
	ref1 := rawState(live66Ref, "ref-r1")
	refC1, err := c.ResumeContent(ctx, live66Ref)
	if err != nil {
		t.Fatalf("ref content: %v", err)
	}
	time.Sleep(15 * time.Second)
	ref2 := rawState(live66Ref, "ref-r2")
	refC2, err := c.ResumeContent(ctx, live66Ref)
	if err != nil {
		t.Fatalf("ref content: %v", err)
	}
	census("ref", ref1, ref2, refC1, refC2)

	// ---- Phase A: problem clone, read-only, two reads ----
	a1 := rawState(live66Clone, "clone-r1")
	c1, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("clone content: %v", err)
	}
	live66Section(t, "clone-r1", c1)
	time.Sleep(15 * time.Second)
	a2 := rawState(live66Clone, "clone-r2")
	c2, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("clone content: %v", err)
	}
	live66Section(t, "clone-r2", c2)
	census("clone", a1, a2, c1, c2)

	// ---- Phase B: no-op title POST (current value), then re-read ----
	title := c1.Resume["title"]
	if len(title) == 0 {
		t.Fatalf("clone has no title; refusing to probe")
	}
	t.Logf("[title-post] sending current title verbatim: %s", title)
	if err := c.postResumeEdit(ctx, live66Clone, "title", title); err != nil {
		t.Fatalf("title no-op POST: %v", err)
	}
	b3 := rawState(live66Clone, "clone-r3")
	c3, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("clone content: %v", err)
	}
	live66Section(t, "clone-r3", c3)
	census("after-title-noop", a2, b3, c2, c3)
}

// TestLive66RefNoopApply is the reference-clone regression probe (task T66
// p.5): a no-op title apply on 4f48f357… must stay applied=true-verified
// (the T51 contract not degraded by the verify scoping) — the POST goes
// through the prod path, the read-back must carry the same title, and the
// clone's experience pool must stay untouched.
//
//	go test -tags live66 -run TestLive66RefNoopApply -count=1 -v ./internal/hhapi/
func TestLive66RefNoopApply(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	before, err := c.ResumeContent(ctx, live66Ref)
	if err != nil {
		t.Fatalf("ref content: %v", err)
	}
	t.Logf("[ref-noop] title=%s exp-sha=%s", before.Resume["title"], live66Sha(before.Profile["experience"]))
	if err := c.postResumeEdit(ctx, live66Ref, "title", before.Resume["title"]); err != nil {
		t.Fatalf("ref no-op POST: %v", err)
	}
	after, err := c.ResumeContent(ctx, live66Ref)
	if err != nil {
		t.Fatalf("ref re-read: %v", err)
	}
	if ok := appliedEqual(after.Resume["title"], before.Resume["title"]); !ok {
		t.Fatalf("ref title verify FAILED: %s vs %s", after.Resume["title"], before.Resume["title"])
	}
	if got, want := live66Sha(after.Profile["experience"]), live66Sha(before.Profile["experience"]); got != want {
		t.Fatalf("ref experience pool changed: %s -> %s", want, got)
	}
	t.Logf("[ref-noop] APPLIED=true (title verified, experience pool constant %s)", live66Sha(after.Profile["experience"]))
}

// TestLive66MarkerCleanup removes the probe's own diagnostic marker from
// the clone's first experience description (the marker rode two
// reproduction saves and the AI rewrite kept the tail). Runs the prod
// apply path scoped to profile.experience.
//
//	go test -tags live66 -run TestLive66MarkerCleanup -count=1 -v ./internal/hhapi/
func TestLive66MarkerCleanup(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	base, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	var exps []map[string]json.RawMessage
	if err := json.Unmarshal(base.Profile["experience"], &exps); err != nil {
		t.Fatalf("experience decode: %v", err)
	}
	cleaned := false
	for _, it := range exps {
		var desc string
		if json.Unmarshal(it["description"], &desc) != nil || !strings.Contains(desc, "[T66-маркер-воспроизведение]") {
			continue
		}
		desc = strings.ReplaceAll(desc, " [T66-маркер-воспроизведение]", "")
		b, _ := json.Marshal(desc)
		it["description"] = b
		cleaned = true
	}
	if !cleaned {
		t.Log("no marker found; already clean")
		return
	}
	mb, _ := json.Marshal(exps)
	mutated := base.Clone()
	mutated.Profile["experience"] = mb
	if err := c.UpdateResumeContent(ctx, live66Clone, mutated, []string{"profile.experience"}); err != nil {
		t.Fatalf("cleanup apply: %v", err)
	}
	final, err := c.ResumeContent(ctx, live66Clone)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	var fin []map[string]json.RawMessage
	if err := json.Unmarshal(final.Profile["experience"], &fin); err != nil {
		t.Fatalf("final decode: %v", err)
	}
	own := 0
	for _, it := range fin {
		var desc string
		_ = json.Unmarshal(it["description"], &desc)
		if strings.Contains(desc, "T66-маркер") {
			t.Fatalf("marker still present")
		}
		if sharedRecordSelf(it, live66Clone) {
			own++
		}
	}
	if own != 3 || len(fin) != 9 {
		t.Fatalf("pool changed: total=%d own=%d, want 9/3", len(fin), own)
	}
	t.Logf("[cleanup] marker removed, pool constant 9/3, applied=true")
}

// live66Section dumps the two symptom sections of a filtered split.
func live66Section(t *testing.T, tag string, rc *ResumeContent) {
	t.Helper()
	for _, key := range []string{"resume.specializationTree", "profile.experience"} {
		side, name := splitContentKey(key)
		raw := sideOf(rc, side)[name]
		s := string(raw)
		if len(s) > 1600 {
			s = s[:1600] + "…"
		}
		t.Logf("[%s] %s: %s", tag, key, s)
	}
}

func live66Sha(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "<nil>"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:12]
}
