package hhapi

// T99 fake tests for DetachDuplicateExperience: the live account shape
// scaled down — one created resume renders every generation of the
// lineage pool (the publish attach), and the dedup must leave exactly
// the newest generation per employer rendering on it with ONE render-set
// POST while every other resume's view stays byte-identical and the pool
// record count never changes. The fake speaks the live render-set
// contract the 2026-10-09 experiment proved: the edit POST's array IS
// the resume's render set.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// dedupSurface fakes the builder state plus the edit route with the live
// render-set semantics. updatePosts counts profile/update calls (the
// dedup must never touch that route — it deletes pool records).
type dedupSurface struct {
	mu          sync.Mutex
	pool        []string // full raw records with resumes membership
	hash        string
	reads       int
	editPosts   int
	updatePosts int
	// dropForeign or dropRecord model integrity accidents for the
	// invariant tests.
	dropForeign bool // a foreign member silently disappears
	dropRecord  bool // a pool record disappears
	dropped     bool
}

func (s *dedupSurface) stateLocked() string {
	items := make([]string, 0, len(s.pool))
	items = append(items, s.pool...)
	if s.dropRecord && s.editPosts > 0 && len(items) > 1 {
		items = items[:len(items)-1] // model: the pool shrinks after the POST
	}
	return `{"nextIncompleteScreenId":null,"screens":[],"resume":{"_attributes":{"hash":"` + s.hash + `","id":"285632195","status":"modified","isSearchable":true}},"profile":{"fields":{"experience":[` + strings.Join(items, ",") + `]}},"additionalProperties":{}}`
}

func (s *dedupSurface) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pool...)
}

func (s *dedupSurface) mount(t *testing.T, env *testEnv, hash string) {
	t.Helper()
	s.hash = hash
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reads++
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.stateLocked()))
	})
	env.serve(env.mux1, "1", "POST", "/applicant/resume/edit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-hhtmSource") != "resume_partial_edit" || r.Header.Get("X-Requested-With") == "" ||
			r.Header.Get("X-Xsrftoken") == "" || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.URL.Query().Get("hhtmSource") != "resume_partial_edit" || r.URL.Query().Get("resume") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body := env.lastBody("1:POST /applicant/resume/edit")
		var envelope struct {
			Experience []struct {
				ID int64 `json:"id"`
			} `json:"experience"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.editPosts++
		want := map[int64]bool{}
		for _, it := range envelope.Experience {
			want[it.ID] = true
		}
		pool := make([]string, 0, len(s.pool))
		for _, raw := range s.pool {
			var p struct {
				ID      int64 `json:"id"`
				Resumes []struct {
					Hash string `json:"hash"`
				} `json:"resumes"`
			}
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			members := make([]string, 0, len(p.Resumes))
			for _, m := range p.Resumes {
				if s.dropForeign && m.Hash != s.hash && len(p.Resumes) > 1 {
					continue // model: a foreign member silently lost
				}
				if m.Hash != s.hash {
					members = append(members, m.Hash)
				}
			}
			if want[p.ID] { // render-set contract: listed records render on self
				members = append(members, s.hash)
			}
			out := make([]json.RawMessage, 0, len(members))
			for _, m := range members {
				out = append(out, json.RawMessage(`{"hash":"`+m+`"}`))
			}
			rec := map[string]json.RawMessage{}
			if err := json.Unmarshal([]byte(raw), &rec); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			rec["resumes"], _ = json.Marshal(out)
			b, _ := json.Marshal(rec)
			pool = append(pool, string(b))
		}
		s.pool = pool
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.stateLocked()))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/profile/update", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.updatePosts++
		s.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest) // must never be called
	})
}

// dedupFixtures builds pool records; owners = resumes rendering it.
func dedupFixture(id int64, company, desc string, owners ...string) string {
	rs := make([]string, 0, len(owners))
	for _, h := range owners {
		rs = append(rs, `{"hash":"`+h+`"}`)
	}
	return `{"id":` + itoa64(id) + `,"startDate":"2024-01-01","endDate":null,"companyName":"` + company + `","position":"Инженер Test","resumes":[` + strings.Join(rs, ",") + `],"description":` + mustJSON(desc) + `}`
}

func dedupMembership(t *testing.T, pool []string, id int64) []string {
	t.Helper()
	for _, raw := range pool {
		var p struct {
			ID      int64 `json:"id"`
			Resumes []struct {
				Hash string `json:"hash"`
			} `json:"resumes"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("fixture decode: %v", err)
		}
		if p.ID != id {
			continue
		}
		owners := make([]string, 0, len(p.Resumes))
		for _, m := range p.Resumes {
			owners = append(owners, m.Hash)
		}
		return owners
	}
	t.Fatalf("record %d not in pool", id)
	return nil
}

func TestDetachDuplicateExperienceKeepsNewestPerEmployer(t *testing.T) {
	const self = "created0"
	surface := &dedupSurface{pool: []string{
		dedupFixture(100, "Компания А", "ген-1", "base1", "clone1", self),
		dedupFixture(110, "Компания А", "ген-2", "clone2", self),
		dedupFixture(120, "Компания А", "ген-3", "clone3", self),
		dedupFixture(200, "Компания Б", "ген-4", "base1", self),
		dedupFixture(210, "Компания Б", "ген-5", "clone2", self),
	}}
	env := newTestEnv(t)
	surface.mount(t, env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if len(res.Kept) != 2 || res.Kept[0] != 120 || res.Kept[1] != 210 {
		t.Fatalf("kept=%v, want [120 210]", res.Kept)
	}
	if len(res.Detached) != 3 {
		t.Fatalf("detached=%v, want 3 ids", res.Detached)
	}
	if surface.editPosts != 1 {
		t.Fatalf("edit posts=%d, want exactly one render-set POST", surface.editPosts)
	}
	if surface.updatePosts != 0 {
		t.Fatalf("the dedup touched the delete route %d times", surface.updatePosts)
	}

	final := surface.snapshot()
	if len(final) != 5 {
		t.Fatalf("pool=%d records, want 5 (the pool is never touched)", len(final))
	}
	for _, id := range []int64{100, 110, 200} {
		owners := dedupMembership(t, final, id)
		for _, o := range owners {
			if o == self {
				t.Fatalf("record %d still renders on self: %v", id, owners)
			}
		}
	}
	// Foreign members untouched on detached records.
	if owners := dedupMembership(t, final, 100); len(owners) != 2 {
		t.Fatalf("record 100 owners=%v, want base1+clone1", owners)
	}
	// Keeps render on self with byte-identical content.
	owners120 := dedupMembership(t, final, 120)
	found := false
	for _, o := range owners120 {
		if o == self {
			found = true
		}
	}
	if !found {
		t.Fatalf("keep 120 lost self: %v", owners120)
	}

	// Idempotency: a second run performs no mutations.
	res2, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("second dedup: %v", err)
	}
	if len(res2.Detached) != 0 || len(res2.Kept) != 2 {
		t.Fatalf("second run kept=%v detached=%v", res2.Kept, res2.Detached)
	}
	if surface.editPosts != 1 {
		t.Fatalf("second run POSTed %d more times", surface.editPosts-1)
	}
}

func TestDetachDuplicateExperienceRestoresCollapsedRenderSet(t *testing.T) {
	const self = "created6"
	// A render set collapsed by an interrupted operation (live: the
	// single-record probe POST): the resume renders only one record. The
	// keep set is selected across the whole pool, so the run restores the
	// full one-per-employer view instead of freezing the collapse.
	surface := &dedupSurface{pool: []string{
		dedupFixture(100, "Компания А", "ген-1", "base1", "clone1"),
		dedupFixture(110, "Компания А", "ген-2", self),
		dedupFixture(120, "Компания А", "ген-3", "clone3"),
		dedupFixture(200, "Компания Б", "ген-4", "base1"),
		dedupFixture(210, "Компания Б", "ген-5", "clone2"),
	}}
	env := newTestEnv(t)
	surface.mount(t, env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if len(res.Kept) != 2 || res.Kept[0] != 120 || res.Kept[1] != 210 {
		t.Fatalf("kept=%v, want [120 210]", res.Kept)
	}
	if len(res.Detached) != 1 || res.Detached[0] != 110 {
		t.Fatalf("detached=%v, want [110]", res.Detached)
	}
	owners120, owners210 := dedupMembership(t, surface.snapshot(), 120), dedupMembership(t, surface.snapshot(), 210)
	for _, owners := range [][]string{owners120, owners210} {
		found := false
		for _, o := range owners {
			if o == self {
				found = true
			}
		}
		if !found {
			t.Fatalf("keep lost self after restore: %v", owners)
		}
	}
	if owners := dedupMembership(t, surface.snapshot(), 110); len(owners) != 0 {
		t.Fatalf("detached 110 still renders on self: %v", owners)
	}
}

func TestDetachDuplicateExperienceNoopWithoutDuplicates(t *testing.T) {
	const self = "created1"
	// The fresh-shell shape: no experience section at all, and the
	// already-deduped shape: one record per employer.
	surface := &dedupSurface{pool: []string{
		dedupFixture(120, "Компания А", "ген-3", "clone3"),
	}}
	env := newTestEnv(t)
	surface.mount(t, env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if len(res.Kept)+len(res.Detached) != 0 {
		t.Fatalf("nothing renders on self — must no-op: %+v", res)
	}
	if surface.editPosts+surface.updatePosts != 0 {
		t.Fatalf("noop mutated: %d posts", surface.editPosts+surface.updatePosts)
	}
}

func TestDetachDuplicateExperienceVerifiesForeignMembership(t *testing.T) {
	const self = "created2"
	// Integrity accident: the render-set POST loses a FOREIGN member of a
	// detached record. The dedup must fail loudly instead of accepting.
	surface := &dedupSurface{pool: []string{
		dedupFixture(100, "Компания А", "ген-1", "base1", "clone1", self),
		dedupFixture(110, "Компания А", "ген-2", "clone2", self),
		dedupFixture(210, "Компания Б", "ген-5", "clone2", self),
	}, dropForeign: true}
	env := newTestEnv(t)
	surface.mount(t, env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	oldInterval, oldBudget := dedupPollInterval, dedupVerifyBudget
	dedupPollInterval, dedupVerifyBudget = 2*time.Millisecond, 50*time.Millisecond
	defer func() { dedupPollInterval, dedupVerifyBudget = oldInterval, oldBudget }()
	ctx, cancel := ctxb(t)
	defer cancel()

	_, err := c.DetachDuplicateExperience(ctx, self)
	if !errors.Is(err, ErrExperienceDedupIntegrity) {
		t.Fatalf("err=%v, want ErrExperienceDedupIntegrity", err)
	}
}

func TestDetachDuplicateExperienceVerifiesPoolSurvival(t *testing.T) {
	const self = "created3"
	// Integrity accident: the pool loses a record around the POST.
	surface := &dedupSurface{pool: []string{
		dedupFixture(100, "Компания А", "ген-1", "base1", "clone1", self),
		dedupFixture(110, "Компания А", "ген-2", "clone2", self),
	}, dropRecord: true}
	env := newTestEnv(t)
	surface.mount(t, env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	if _, err := c.DetachDuplicateExperience(ctx, self); !errors.Is(err, ErrExperienceDedupIntegrity) {
		t.Fatalf("err=%v, want ErrExperienceDedupIntegrity", err)
	}
}
