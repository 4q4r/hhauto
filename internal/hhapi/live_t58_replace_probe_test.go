//go:build live58

package hhapi

// T58 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live58 -run TestLive58ExperienceReplaceProbe -count=1 -v ./internal/hhapi/
//
// Drives the experience_description applies of the LIVE clone through the
// PROD replace flow (UpdateResumeContent with WithExperienceReplace(true))
// against the REAL hh contracts. The edit contract is upsert-by-id; hh
// answers content edits with an IN-PLACE update or a FORK (new id, old
// record stays — the T51/T55 duplicate growth), per apply timing. The flow
// must hold BOTH modes:
//
//	in-place → the replace detection finds nothing replaced, deletes nothing;
//	fork     → the replaced old generation is deleted, the count stays.
//
// Clone 4f48f357… is the polygon (clean after T56: 3 own + 3 parent
// records). Phases (re-runnable — the marker is stripped from the
// descriptions to recover the originals):
//
//	A: mark ALL own descriptions in ONE apply (the real improve shape) →
//	   pool total stays 6, own stays 3, every own record carries the
//	   marker, parent records byte-identical; if hh forked, the vanished
//	   old ids must be DELETED from the pool (the replace flow's job).
//	B: no-op repeat → pool byte-identical (idempotency).
//	C: restore the original descriptions in ONE apply → total 6, own 3,
//	   descriptions byte-original, no duplicates, parents untouched.
//
// Base resumes are never touched: every write addresses the clone hash
// only, deletes are bound to resumeFromHash + own record ids.
//
// No secrets are printed; env vars are not required (defaults below).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const (
	live58DefaultHash  = "4f48f357ff112c2fcc0039ed1f6c7555414349"
	live58MarkerSuffix = " Проверка замещения T58 (контракт fork+delete)."
)

type live58Rec struct {
	ID          int64  `json:"id"`
	Company     string `json:"companyName"`
	Position    string `json:"position"`
	Description string `json:"description"`
	Resumes     []struct {
		Hash string `json:"hash"`
	} `json:"resumes"`
}

func live58SHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func TestLive58ExperienceReplaceProbe(t *testing.T) {
	hash := os.Getenv("HHAUTO_T58_HASH")
	if hash == "" {
		hash = live58DefaultHash
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
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil,
		WithBrowserTLSSpec(spec), WithExperienceReplace(true))
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
	ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	readPool := func() []live58Rec {
		t.Helper()
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("builder state: %v", err)
		}
		pool, err := experiencePool(st)
		if err != nil {
			t.Fatalf("experience pool: %v", err)
		}
		out := make([]live58Rec, 0, len(pool))
		for _, rec := range pool {
			var r live58Rec
			if err := json.Unmarshal(rec.Raw, &r); err != nil {
				t.Fatalf("record decode: %v", err)
			}
			out = append(out, r)
		}
		return out
	}
	owns := func(pool []live58Rec) []live58Rec {
		var out []live58Rec
		for _, r := range pool {
			if len(r.Resumes) == 1 && r.Resumes[0].Hash == hash {
				out = append(out, r)
			}
		}
		return out
	}
	parents := func(pool []live58Rec) []live58Rec {
		var out []live58Rec
		for _, r := range pool {
			if len(r.Resumes) != 1 || r.Resumes[0].Hash != hash {
				out = append(out, r)
			}
		}
		return out
	}
	dump := func(tag string, pool []live58Rec) {
		for _, r := range pool {
			kind := "own"
			if len(r.Resumes) != 1 || r.Resumes[0].Hash != hash {
				kind = "parent"
			}
			t.Logf("%s id=%d %s %s / %s desc_len=%d desc_sha=%s resumes=%d",
				tag, r.ID, kind, r.Company, r.Position, len(r.Description), live58SHA(r.Description), len(r.Resumes))
		}
	}
	ids := func(pool []live58Rec) map[int64]bool {
		out := map[int64]bool{}
		for _, r := range pool {
			out[r.ID] = true
		}
		return out
	}
	sameRaw := func(a, b live58Rec) bool {
		ab, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		return string(ab) == string(bb)
	}
	parentsIntact := func(tag string, before, after []live58Rec) {
		t.Helper()
		if len(before) != len(after) {
			t.Fatalf("%s: parent count %d -> %d", tag, len(before), len(after))
		}
		for _, p := range before {
			ok := false
			for _, r := range after {
				if r.ID != p.ID {
					continue
				}
				// The usage-list ORDER is server-unstable (live 2026-09-29:
				// the same 5 hashes came back reordered between reads) —
				// compare it as a set; everything else byte-strict.
				if p.Description != r.Description || p.Company != r.Company || p.Position != r.Position {
					continue
				}
				if len(p.Resumes) != len(r.Resumes) {
					continue
				}
				hashes := map[string]int{}
				balanced := true
				for _, h := range p.Resumes {
					hashes[h.Hash]++
				}
				for _, h := range r.Resumes {
					hashes[h.Hash]--
					if hashes[h.Hash] < 0 {
						balanced = false
					}
				}
				if balanced {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("%s: parent record id=%d changed", tag, p.ID)
			}
		}
	}
	// applyDescriptions rewrites the split's own records through the given
	// map (id → new description) and runs the prod replace flow.
	applyDescriptions := func(descByID map[int64]string) {
		t.Helper()
		live, err := c.ResumeContent(ctx, hash)
		if err != nil {
			t.Fatalf("live content: %v", err)
		}
		content := live.Clone()
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(content.Profile["experience"], &items); err != nil {
			t.Fatalf("decode split experience: %v", err)
		}
		changed := 0
		for i := range items {
			var id int64
			if json.Unmarshal(items[i]["id"], &id) != nil {
				continue
			}
			desc, ok := descByID[id]
			if !ok {
				continue
			}
			items[i]["description"] = json.RawMessage(mustJSON(desc))
			changed++
		}
		if changed != len(descByID) {
			t.Fatalf("apply: only %d of %d targets found among own records", changed, len(descByID))
		}
		updated, _ := json.Marshal(items)
		content.Profile["experience"] = updated
		if err := c.UpdateResumeContent(ctx, hash, content, nil); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	start := readPool()
	if len(start) != 6 || len(owns(start)) != 3 {
		t.Fatalf("unexpected start state: total=%d own=%d (want 6/3 — run only on the clean T56 state)", len(start), len(owns(start)))
	}
	startParents := parents(start)

	// Originals: the current own descriptions with any leftover probe
	// marker stripped (re-runnable). unmarked counts the records still
	// needing the marker.
	originals := map[int64]string{}
	unmarked := 0
	for _, r := range owns(start) {
		orig := r.Description
		if strings.HasSuffix(orig, live58MarkerSuffix) {
			orig = strings.TrimSuffix(orig, live58MarkerSuffix)
		} else {
			unmarked++
		}
		originals[r.ID] = orig
	}
	t.Logf("start: %d own records, %d already marked (re-run recovery)", len(originals), len(originals)-unmarked)

	// --- Phase A: mark every own description in ONE apply ---
	wantMarked := map[int64]string{}
	for id, orig := range originals {
		wantMarked[id] = orig + live58MarkerSuffix
	}
	if unmarked > 0 {
		t.Logf("phase A: applying the marker on %d own record(s) through the prod replace flow…", unmarked)
		applyDescriptions(wantMarked)
	}
	afterA := readPool()
	dump("A", afterA)
	if len(afterA) != 6 || len(owns(afterA)) != 3 {
		t.Fatalf("phase A: pool total=%d own=%d, want 6/3 (no growth)", len(afterA), len(owns(afterA)))
	}
	for _, r := range owns(afterA) {
		want, ok := wantMarked[r.ID]
		if ok && r.Description == want {
			continue // in-place survivor
		}
		matched := false
		for _, wantID := range wantMarked {
			if r.Description == wantID {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("phase A: own record id=%d carries neither its marked nor expected description", r.ID)
		}
	}
	// Fork mode: any pre-apply own id that vanished must be GONE from the
	// pool entirely (the replace flow deleted the replaced generation).
	forked := false
	for id := range originals {
		if !ids(start)[id] || ids(afterA)[id] {
			continue
		}
		forked = true
		t.Logf("phase A: hh FORKED id=%d — the replace flow deleted the old generation", id)
	}
	if forked {
		t.Logf("phase A mode: FORK+DELETE (the duplicate-growth path is covered live)")
	} else {
		t.Logf("phase A mode: IN-PLACE (hh answered with same-id updates; the flow correctly deleted nothing)")
	}
	parentsIntact("phase A", startParents, parents(afterA))
	t.Logf("phase A PASS: total=6 own=3, no duplicates")

	// --- Phase B: no-op repeat ---
	live2, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("phase B read: %v", err)
	}
	if err := c.UpdateResumeContent(ctx, hash, live2.Clone(), nil); err != nil {
		t.Fatalf("phase B apply: %v", err)
	}
	afterB := readPool()
	if len(afterB) != 6 || len(owns(afterB)) != 3 {
		t.Fatalf("phase B: pool moved: total=%d own=%d", len(afterB), len(owns(afterB)))
	}
	for _, r := range owns(afterB) {
		for _, was := range owns(afterA) {
			if was.ID == r.ID && !sameRaw(was, r) {
				t.Fatalf("phase B: own record id=%d changed on a no-op repeat", r.ID)
			}
		}
	}
	parentsIntact("phase B", parents(afterA), parents(afterB))
	t.Logf("phase B PASS: idempotent no-op, pool untouched")

	// --- Phase C: restore the originals in ONE apply ---
	t.Logf("phase C: restoring the original descriptions through the prod replace flow…")
	applyDescriptions(originals)
	afterC := readPool()
	dump("C", afterC)
	if len(afterC) != 6 || len(owns(afterC)) != 3 {
		t.Fatalf("phase C: pool total=%d own=%d, want 6/3", len(afterC), len(owns(afterC)))
	}
	restored := 0
	for _, r := range owns(afterC) {
		orig, ok := originals[r.ID]
		if ok && r.Description == orig {
			restored++
			continue
		}
		for _, want := range originals {
			if r.Description == want {
				restored++ // fork carrier with the same content under a new id
			}
		}
	}
	if restored != 3 {
		t.Fatalf("phase C: only %d/3 own descriptions restored byte-for-byte", restored)
	}
	// No duplicate job blocks: every (company, position) among own records unique.
	seen := map[string]bool{}
	for _, r := range owns(afterC) {
		key := r.Company + "\x00" + r.Position
		if seen[key] {
			t.Fatalf("phase C: duplicate job block %q among own records", key)
		}
		seen[key] = true
	}
	parentsIntact("phase C", startParents, parents(afterC))
	t.Logf("LIVE58 RESULT: replace flow verified — counter constant at 6, no duplicates, originals byte-for-byte; fork mode=%v", forked)
}
