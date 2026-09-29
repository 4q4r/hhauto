package hhapi

// T58 experience-replace flow tests: the edit contract is upsert-by-id
// (live T55) — every edited experience record FORKS (a new id carries the
// new content while the old record stays), so the resume grows duplicate
// blocks. The replace flow (UpdateResumeContent with
// [features.autoclone.improve] experience_replace) deletes the replaced
// old generations through the T56 pool-replace route and re-checks the
// flow invariant: own record count constant, keep records byte-identical,
// sent targets present.
//
// The fake speaks both live contracts: the edit POST forks content edits
// (unknown id → add, unchanged rider → no-op), the profile/update POST
// deletes every id missing from its array (REPLACE, T56) — or silently
// no-ops (the live T55 failure mode) to prove the error surfaces.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// t58Record renders one pool record; owners lists the hashes the record
// renders on (a record owned by another hash only is inherited).
func t58Record(id int64, desc string, owners ...string) string {
	rs := make([]string, 0, len(owners))
	for _, h := range owners {
		rs = append(rs, `{"hash":"`+h+`"}`)
	}
	return `{"id":` + itoa64(id) + `,"startDate":"2024-01-01","endDate":null,"companyName":"Компания Test","position":"Инженер Test","resumes":[` + strings.Join(rs, ",") + `],"description":` + mustJSON(desc) + `}`
}

// replaceSurface fakes the builder state plus both mutation routes.
type replaceSurface struct {
	mu           sync.Mutex
	pool         []string
	nextID       int64
	hash         string
	silentUpdate bool // profile/update answers 200 but applies nothing
	editPosts    int
	updatePosts  int
}

func (s *replaceSurface) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pool...)
}

func (s *replaceSurface) stateLocked() string {
	return `{"nextIncompleteScreenId":null,"screens":[],"resume":{"_attributes":{"hash":"` + s.hash + `","id":"285632195","status":"modified","isSearchable":true}},"profile":{"fields":{"experience":[` + strings.Join(s.pool, ",") + `]}},"additionalProperties":{}}`
}

func (s *replaceSurface) mount(env *testEnv, hash string) {
	s.hash = hash
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.stateLocked()))
	})
	env.serve(env.mux1, "1", "POST", "/applicant/resume/edit", func(w http.ResponseWriter, r *http.Request) {
		// The captured T51 UI header set; missing pieces fail the test.
		if r.Header.Get("X-hhtmSource") != "resume_partial_edit" || r.Header.Get("X-Requested-With") == "" ||
			r.Header.Get("X-Xsrftoken") == "" || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.URL.Query().Get("hhtmSource") != "resume_partial_edit" || r.URL.Query().Get("resume") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body := env.lastBody("1:POST /applicant/resume/edit")
		var req struct {
			Experience []map[string]json.RawMessage `json:"experience"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.editPosts++
		for _, item := range req.Experience {
			var id int64
			_ = json.Unmarshal(item["id"], &id)
			idx := t58PoolIndex(s.pool, id)
			switch {
			case id == 0 || idx < 0: // unknown id → add (live T55 upsert semantics)
				s.pool = append(s.pool, t58WithMeta(item, s.hash, 0))
			case !t58SameContent(s.pool[idx], item): // content edit → FORK: new id, old record stays
				s.nextID++
				s.pool = append(s.pool, t58WithMeta(item, s.hash, s.nextID))
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.stateLocked()))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/profile/update", func(w http.ResponseWriter, r *http.Request) {
		// The live T56 header set; missing pieces fail the test loudly.
		if r.Header.Get("X-hhtmSource") != "profile_experience" || r.Header.Get("X-hhtmFrom") != "resume" ||
			r.Header.Get("X-hhtmSourceLabel") != "resume" || r.Header.Get("X-hhtmFromLabel") != "resume" ||
			r.Header.Get("X-Xsrftoken") == "" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Sec-Fetch-Site") != "same-site" || r.Header.Get("Origin") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body := env.lastBody("1:POST /profile/shards/profile/update")
		var req struct {
			Profile struct {
				Experience []json.RawMessage `json:"experience"`
			} `json:"profile"`
			ResumeFromHash string `json:"resumeFromHash"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil || req.ResumeFromHash != s.hash {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.updatePosts++
		if !s.silentUpdate { // REPLACE: ids missing from the array are deleted
			pool := make([]string, 0, len(req.Profile.Experience))
			for _, raw := range req.Profile.Experience {
				pool = append(pool, string(raw))
			}
			s.pool = pool
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"NEW"}`))
	})
}

func t58PoolIndex(pool []string, id int64) int {
	for i, raw := range pool {
		var p struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal([]byte(raw), &p) == nil && p.ID == id {
			return i
		}
	}
	return -1
}

// t58WithMeta copies a sent item (which never carries server metadata)
// into a pool record: the resumes usage list is the target resume and,
// when forkID > 0, a fresh server id replaces the sent one.
func t58WithMeta(item map[string]json.RawMessage, self string, forkID int64) string {
	rec := make(map[string]json.RawMessage, len(item)+1)
	for k, v := range item {
		rec[k] = v
	}
	rec["resumes"] = json.RawMessage(`[{"hash":"` + self + `"}]`)
	if forkID > 0 {
		rec["id"] = json.RawMessage(itoa64(forkID))
	}
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// t58SameContent compares a pool record with a sent item ignoring the
// server-only metadata the split strips (resumes, companyLogos).
func t58SameContent(poolRaw string, item map[string]json.RawMessage) bool {
	strip := func(m map[string]json.RawMessage) map[string]json.RawMessage {
		out := make(map[string]json.RawMessage, len(m))
		for k, v := range m {
			if k == "resumes" || k == "companyLogos" {
				continue
			}
			out[k] = v
		}
		return out
	}
	var pool map[string]json.RawMessage
	if err := json.Unmarshal([]byte(poolRaw), &pool); err != nil {
		return false
	}
	pb, err1 := json.Marshal(strip(pool))
	ib, err2 := json.Marshal(strip(item))
	return err1 == nil && err2 == nil && rawEqual(pb, ib)
}

// t58SetDescription rewrites one own record's description in the split —
// the feature-shaped experience_description mutation.
func t58SetDescription(t *testing.T, rc *ResumeContent, idWant int64, desc string) {
	t.Helper()
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(rc.Profile["experience"], &items); err != nil {
		t.Fatalf("decode experience: %v", err)
	}
	hit := false
	for i := range items {
		var id int64
		if json.Unmarshal(items[i]["id"], &id) != nil || id != idWant {
			continue
		}
		items[i]["description"] = json.RawMessage(mustJSON(desc))
		hit = true
	}
	if !hit {
		t.Fatalf("record id=%d not among own records", idWant)
	}
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("encode experience: %v", err)
	}
	rc.Profile["experience"] = b
}

func replaceEndpoints(env *testEnv, opts ...ClientOption) *Client {
	env.t.Helper()
	all := append([]ClientOption{WithBrowserTLSSpec(captureSpec(env.t))}, opts...)
	c, err := NewClient(env.account(), env.endpoints(), nil, nil, all...)
	if err != nil {
		env.t.Fatalf("NewClient: %v", err)
	}
	c.setXSRFForTests("tok_test")
	return c
}

// t58OwnIDs returns the ids of the records rendering on self.
func t58OwnIDs(t *testing.T, pool []string, self string) map[int64]bool {
	t.Helper()
	out := map[int64]bool{}
	for _, raw := range pool {
		var rec struct {
			ID      int64 `json:"id"`
			Resumes []struct {
				Hash string `json:"hash"`
			} `json:"resumes"`
		}
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("decode pool record: %v", err)
		}
		if len(rec.Resumes) == 1 && rec.Resumes[0].Hash == self {
			out[rec.ID] = true
		}
	}
	return out
}

// The core scenario: an applied experience_description forks the record
// (new id, old stays) — the replace flow deletes the replaced old record,
// own count stays constant, keeps and inherited records ride byte-for-byte.
func TestUpdateResumeContentReplacesForkedExperience(t *testing.T) {
	const hash = "cl0ne58a"
	own102 := t58Record(102, "Описание сто два", hash)
	own103 := t58Record(103, "Описание сто три", hash)
	inh901 := t58Record(901, "Родительское 901", "other58x")
	inh902 := t58Record(902, "Родительское 902", "other58x")
	surface := &replaceSurface{nextID: 2000, pool: []string{
		t58Record(101, "Описание сто один", hash),
		own102, own103, inh901, inh902,
	}}
	env := newTestEnv(t)
	surface.mount(env, hash)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t58SetDescription(t, content, 101, "Обновлённое описание 101")
	if err := c.UpdateResumeContent(ctx, hash, content, nil); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}

	pool := surface.snapshot()
	if len(pool) != 5 {
		t.Fatalf("pool has %d records, want 5 (the fork replaced the old record, no growth)", len(pool))
	}
	if t58PoolIndex(pool, 101) >= 0 {
		t.Fatal("replaced record 101 still in the pool")
	}
	own := t58OwnIDs(t, pool, hash)
	if len(own) != 3 || !own[102] || !own[103] {
		t.Fatalf("own records = %v, want {102,103,fork}", own)
	}
	forkID := int64(0)
	for _, raw := range pool {
		var rec struct {
			ID          int64  `json:"id"`
			Description string `json:"description"`
		}
		if json.Unmarshal([]byte(raw), &rec) != nil || !own[rec.ID] || rec.ID == 102 || rec.ID == 103 {
			continue
		}
		forkID = rec.ID
		if rec.Description != "Обновлённое описание 101" {
			t.Fatalf("fork description = %q", rec.Description)
		}
	}
	if forkID == 0 {
		t.Fatal("no fork found among own records")
	}
	// Keeps and inherited records ride byte-for-byte.
	for i, want := range []string{own102, own103, inh901, inh902} {
		idx := t58PoolIndex(pool, []int64{102, 103, 901, 902}[i])
		if idx < 0 || pool[idx] != want {
			t.Fatalf("record %d changed: got %s", []int64{102, 103, 901, 902}[i], pool[idx])
		}
	}
}

// Inherited (parent-resume) records never ride the split, so they are
// never replaced and never deleted — the shared-record guard must not even
// be reached.
func TestUpdateResumeContentReplaceNeverTouchesInherited(t *testing.T) {
	const hash = "cl0ne58b"
	inh901 := t58Record(901, "Родительское 901", "other58y")
	surface := &replaceSurface{nextID: 2000, pool: []string{
		t58Record(101, "Описание сто один", hash),
		inh901,
	}}
	env := newTestEnv(t)
	surface.mount(env, hash)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t58SetDescription(t, content, 101, "Обновлённое описание 101")
	if err := c.UpdateResumeContent(ctx, hash, content, nil); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}
	pool := surface.snapshot()
	if len(pool) != 2 {
		t.Fatalf("pool has %d records, want 2", len(pool))
	}
	idx := t58PoolIndex(pool, 901)
	if idx < 0 || pool[idx] != inh901 {
		t.Fatal("inherited record 901 must ride byte-for-byte")
	}
	own := t58OwnIDs(t, pool, hash)
	if len(own) != 1 || own[101] {
		t.Fatalf("own records = %v, want the fork only", own)
	}
}

// experience_replace off (or the option never wired): the raw T51/T55
// behavior — the fork stays, no delete POSTs, and the strict verify gate
// cannot accept the fork-grown read-back (it polls to the budget and
// fails: the T55 "hangs on polling" fact), so nothing claims success.
func TestUpdateResumeContentReplaceDisabledKeepsForks(t *testing.T) {
	origBudget, origPoll := verifyWaitBudget, verifyPollInterval
	defer func() { verifyWaitBudget, verifyPollInterval = origBudget, origPoll }()
	verifyWaitBudget = 100 * time.Millisecond
	verifyPollInterval = 20 * time.Millisecond
	const hash = "cl0ne58c"
	surface := &replaceSurface{nextID: 2000, pool: []string{
		t58Record(101, "Описание сто один", hash),
		t58Record(102, "Описание сто два", hash),
	}}
	env := newTestEnv(t)
	surface.mount(env, hash)
	c := replaceEndpoints(env) // no WithExperienceReplace
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t58SetDescription(t, content, 101, "Обновлённое описание 101")
	err = c.UpdateResumeContent(ctx, hash, content, nil)
	var ve *ResumeContentVerifyError
	if !errors.As(err, &ve) || !slices.Contains(ve.Keys, "profile.experience") {
		t.Fatalf("err = %v, want verify failure on profile.experience", err)
	}
	pool := surface.snapshot()
	if len(pool) != 3 {
		t.Fatalf("pool has %d records, want 3 (the fork must stay when replace is off)", len(pool))
	}
	if t58PoolIndex(pool, 101) < 0 {
		t.Fatal("old record 101 must stay when replace is off")
	}
	if surface.updatePosts != 0 {
		t.Fatalf("replace off must not POST profile/update, got %d", surface.updatePosts)
	}
}

// Repeated improves over the same records are safe: the second replace
// cycle forks the current generation and deletes exactly that one old
// record (nothing else — the replaced set only ever holds ids this apply
// edited), and a no-diff repeat posts and deletes nothing.
func TestUpdateResumeContentReplaceIdempotentRepeat(t *testing.T) {
	const hash = "cl0ne58d"
	surface := &replaceSurface{nextID: 2000, pool: []string{
		t58Record(101, "Описание сто один", hash),
		t58Record(102, "Описание сто два", hash),
	}}
	env := newTestEnv(t)
	surface.mount(env, hash)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	// Cycle 1: 101 → fork1 (2001), 101 deleted.
	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t58SetDescription(t, content, 101, "Текст поколения 2")
	if err := c.UpdateResumeContent(ctx, hash, content, nil); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	pool := surface.snapshot()
	if len(pool) != 2 || t58PoolIndex(pool, 101) >= 0 {
		t.Fatalf("after cycle 1: pool=%d, 101 present=%v", len(pool), t58PoolIndex(pool, 101) >= 0)
	}
	edits1, updates1 := surface.editPosts, surface.updatePosts

	// Cycle 2: improve re-reads live (fork ids now), changes the fork —
	// exactly one more replace, count constant.
	live2, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent 2: %v", err)
	}
	forkID := int64(0)
	for id := range t58OwnIDs(t, surface.snapshot(), hash) {
		if id != 102 {
			forkID = id
		}
	}
	if forkID == 0 {
		t.Fatal("fork1 not found")
	}
	content2 := live2.Clone()
	t58SetDescription(t, content2, forkID, "Текст поколения 3")
	if err := c.UpdateResumeContent(ctx, hash, content2, nil); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	pool = surface.snapshot()
	if len(pool) != 2 {
		t.Fatalf("after cycle 2: pool=%d, want 2 (no growth)", len(pool))
	}
	if t58PoolIndex(pool, forkID) >= 0 {
		t.Fatal("replaced fork1 still in the pool")
	}
	own := t58OwnIDs(t, pool, hash)
	if len(own) != 2 || !own[102] {
		t.Fatalf("own records = %v, want {102,fork2}", own)
	}

	// Cycle 3: an unchanged repeat is a pure no-op — no POSTs at all.
	live3, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent 3: %v", err)
	}
	if err := c.UpdateResumeContent(ctx, hash, live3.Clone(), nil); err != nil {
		t.Fatalf("apply 3: %v", err)
	}
	if surface.editPosts != edits1+1 || surface.updatePosts != updates1+1 {
		t.Fatalf("no-op repeat posted: edits %d→%d updates %d→%d",
			edits1, surface.editPosts, updates1, surface.updatePosts)
	}
	before := surface.snapshot()
	if err := c.UpdateResumeContent(ctx, hash, live3.Clone(), nil); err != nil {
		t.Fatalf("apply 4: %v", err)
	}
	after := surface.snapshot()
	if strings.Join(before, "|") != strings.Join(after, "|") {
		t.Fatal("no-op repeat must not touch the pool")
	}
}

// A delete hh accepted but silently ignored (the live T55 failure mode)
// surfaces as the apply-layer error — the resume visibly keeps the
// duplicates instead of a silent claim of success.
func TestUpdateResumeContentReplaceDeleteFailureSurfaces(t *testing.T) {
	const hash = "cl0ne58e"
	surface := &replaceSurface{nextID: 2000, silentUpdate: true, pool: []string{
		t58Record(101, "Описание сто один", hash),
		t58Record(102, "Описание сто два", hash),
	}}
	env := newTestEnv(t)
	surface.mount(env, hash)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t58SetDescription(t, content, 101, "Обновлённое описание 101")
	err = c.UpdateResumeContent(ctx, hash, content, nil)
	if err == nil {
		t.Fatal("silent delete no-op must surface as an error")
	}
	if !errors.Is(err, ErrExperienceDelete) {
		t.Fatalf("err=%v, want ErrExperienceDelete in the chain", err)
	}
	pool := surface.snapshot()
	if t58PoolIndex(pool, 101) < 0 {
		t.Fatal("the replaced old record must stay visible when the delete failed")
	}
	if len(pool) != 3 {
		t.Fatalf("pool=%d, want 3 (fork + old + keep: the duplicates stay visible)", len(pool))
	}
}
