package hhapi

// T99 fake tests for DetachDuplicateExperience: the live account shape
// scaled down — one created resume renders every generation of the
// lineage pool (the publish attach), and the dedup must leave exactly the
// newest generation per employer rendering on it while every other
// resume's view stays byte-identical and the pool record count is
// constant (fork + delete-fork nets zero).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// dedupSpec builds one fake pool record; company groups the dedup.
type dedupSpec struct {
	id      int64
	company string
	owners  []string
	desc    string
}

func dedupRecord(t *testing.T, s dedupSpec) string {
	t.Helper()
	rs := make([]string, 0, len(s.owners))
	for _, h := range s.owners {
		rs = append(rs, `{"hash":"`+h+`"}`)
	}
	return `{"id":` + itoa64(s.id) + `,"startDate":"2024-01-01","endDate":null,"companyName":"` + s.company + `","position":"Инженер Test","resumes":[` + strings.Join(rs, ",") + `],"description":` + mustJSON(s.desc) + `}`
}

func dedupMembership(t *testing.T, pool []string, id int64) (owners []string, desc string) {
	t.Helper()
	for _, raw := range pool {
		var p struct {
			ID          int64    `json:"id"`
			Description string   `json:"description"`
			Resumes     []string `json:"-"`
		}
		var probe struct {
			Resumes []struct {
				Hash string `json:"hash"`
			} `json:"resumes"`
		}
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			t.Fatalf("fixture decode: %v", err)
		}
		_ = json.Unmarshal([]byte(raw), &p)
		if p.ID != id {
			continue
		}
		for _, r := range probe.Resumes {
			owners = append(owners, r.Hash)
		}
		return owners, p.Description
	}
	t.Fatalf("record %d not in pool", id)
	return nil, ""
}

func TestDetachDuplicateExperienceKeepsNewestPerEmployer(t *testing.T) {
	const self = "created0"
	// Two employers × generations; self renders all five (the publish
	// attach), the newest generation per employer is the highest id.
	pool := &[]string{
		dedupRecord(t, dedupSpec{100, "Компания А", []string{"base1", "clone1", self}, "ген-1"}),
		dedupRecord(t, dedupSpec{110, "Компания А", []string{"clone2", self}, "ген-2"}),
		dedupRecord(t, dedupSpec{120, "Компания А", []string{"clone3", self}, "ген-3"}),
		dedupRecord(t, dedupSpec{200, "Компания Б", []string{"base1", self}, "ген-4"}),
		dedupRecord(t, dedupSpec{210, "Компания Б", []string{"clone2", self}, "ген-5"}),
	}
	surface := &replaceSurface{nextID: 2000, reassignShared: true, pool: *pool}
	env := newTestEnv(t)
	surface.mount(env, self)
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
	if len(res.Skipped) != 0 {
		t.Fatalf("skipped=%v, want none", res.Skipped)
	}

	final := surface.snapshot()
	if len(final) != 5 {
		t.Fatalf("pool=%d records, want 5 (fork+delete nets zero)", len(final))
	}
	// The detached originals dropped self but kept everyone else and
	// their content byte-identical.
	wantDesc := map[int64]string{100: "ген-1", 110: "ген-2", 200: "ген-4"}
	for id, want := range wantDesc {
		owners, desc := dedupMembership(t, final, id)
		if strings.Contains(strings.Join(owners, ","), self) {
			t.Fatalf("record %d still renders on self: %v", id, owners)
		}
		if desc != want {
			t.Fatalf("record %d desc=%q, want %q", id, desc, want)
		}
	}
	// Keeps still render on self.
	for _, id := range []int64{120, 210} {
		owners, _ := dedupMembership(t, final, id)
		found := false
		for _, o := range owners {
			if o == self {
				found = true
			}
		}
		if !found {
			t.Fatalf("keep %d no longer renders on self: %v", id, owners)
		}
	}
	// Other owners untouched on detached records.
	owners100, _ := dedupMembership(t, final, 100)
	if len(owners100) != 2 || owners100[0] != "base1" || owners100[1] != "clone1" {
		t.Fatalf("record 100 owners=%v, want base1+clone1", owners100)
	}
	if surface.editPosts != 3 || surface.updatePosts != 3 {
		t.Fatalf("posts: edit=%d update=%d, want 3/3", surface.editPosts, surface.updatePosts)
	}

	// Idempotency: a second run performs no mutations.
	postsBefore := surface.editPosts + surface.updatePosts
	res2, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("second dedup: %v", err)
	}
	if len(res2.Kept) != 2 || len(res2.Detached) != 0 {
		t.Fatalf("second run kept=%v detached=%v", res2.Kept, res2.Detached)
	}
	if surface.editPosts+surface.updatePosts != postsBefore {
		t.Fatalf("second run mutated: %d posts", surface.editPosts+surface.updatePosts-postsBefore)
	}
}

func TestDetachDuplicateExperienceWaitsForLaggingFork(t *testing.T) {
	const self = "created1"
	pool := &[]string{
		dedupRecord(t, dedupSpec{100, "Компания А", []string{"base1", self}, "старый"}),
		dedupRecord(t, dedupSpec{110, "Компания А", []string{"clone1", self}, "новый"}),
	}
	surface := &replaceSurface{nextID: 3000, reassignShared: true, forkAfterReads: 3, pool: *pool}
	env := newTestEnv(t)
	surface.mount(env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	oldInterval, oldBudget := dedupPollInterval, dedupForkBudget
	dedupPollInterval, dedupForkBudget = 2*time.Millisecond, 5*time.Second
	defer func() { dedupPollInterval, dedupForkBudget = oldInterval, oldBudget }()
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup with lagging fork: %v", err)
	}
	if len(res.Detached) != 1 || res.Detached[0] != 100 {
		t.Fatalf("detached=%v, want [100]", res.Detached)
	}
	final := surface.snapshot()
	if len(final) != 2 {
		t.Fatalf("pool=%d, want 2", len(final))
	}
	owners, _ := dedupMembership(t, final, 100)
	for _, o := range owners {
		if o == self {
			t.Fatalf("record 100 still renders on self after lagged fork: %v", owners)
		}
	}
}

func TestDetachDuplicateExperienceDeletesCrashOrphan(t *testing.T) {
	const self = "created2"
	// A previous run crashed between the fork POST and the fork delete:
	// the tweaked fork renders lone. It must be deleted directly (sole
	// member), leaving the shared original as the keep.
	pool := &[]string{
		dedupRecord(t, dedupSpec{100, "Компания А", []string{"base1", self}, "текст"}),
		dedupRecord(t, dedupSpec{150, "Компания А", []string{self}, "текст\n"}),
	}
	surface := &replaceSurface{nextID: 4000, reassignShared: true, pool: *pool}
	env := newTestEnv(t)
	surface.mount(env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if len(res.Detached) != 1 || res.Detached[0] != 150 {
		t.Fatalf("detached=%v, want [150]", res.Detached)
	}
	if len(res.Kept) != 1 || res.Kept[0] != 100 {
		t.Fatalf("kept=%v, want [100]", res.Kept)
	}
	if surface.editPosts != 0 {
		t.Fatalf("orphan delete must not fork: %d edit posts", surface.editPosts)
	}
	final := surface.snapshot()
	if len(final) != 1 {
		t.Fatalf("pool=%d, want 1", len(final))
	}
	owners, desc := dedupMembership(t, final, 100)
	if len(owners) != 2 || desc != "текст" {
		t.Fatalf("keep record changed: owners=%v desc=%q", owners, desc)
	}
}

func TestDetachDuplicateExperienceSkipsUnknownLoneRecords(t *testing.T) {
	const self = "created3"
	// A lone record that is not this mechanism's orphan (no tweak
	// signature) is never touched.
	pool := &[]string{
		dedupRecord(t, dedupSpec{100, "Компания А", []string{"base1", self}, "общий"}),
		dedupRecord(t, dedupSpec{160, "Компания А", []string{self}, "своё"}),
	}
	surface := &replaceSurface{nextID: 4000, pool: *pool}
	env := newTestEnv(t)
	surface.mount(env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != 160 {
		t.Fatalf("skipped=%v, want [160]", res.Skipped)
	}
	if len(res.Kept) != 1 || res.Kept[0] != 100 || len(res.Detached) != 0 {
		t.Fatalf("kept=%v detached=%v", res.Kept, res.Detached)
	}
	if surface.editPosts+surface.updatePosts != 0 {
		t.Fatalf("skip path mutated: %d posts", surface.editPosts+surface.updatePosts)
	}
}

func TestDetachDuplicateExperienceNoopWithoutDuplicates(t *testing.T) {
	const self = "created4"
	// The fresh-shell shape: nothing of the pool renders on self.
	pool := &[]string{
		dedupRecord(t, dedupSpec{100, "Компания А", []string{"base1"}, "ген-1"}),
	}
	surface := &replaceSurface{nextID: 4000, pool: *pool}
	env := newTestEnv(t)
	surface.mount(env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	res, err := c.DetachDuplicateExperience(ctx, self)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if len(res.Kept)+len(res.Detached)+len(res.Skipped) != 0 {
		t.Fatalf("empty view must no-op: %+v", res)
	}
	if surface.editPosts+surface.updatePosts != 0 {
		t.Fatalf("noop mutated: %d posts", surface.editPosts+surface.updatePosts)
	}
}

func TestDetachDuplicateExperienceRevertsInPlaceEdit(t *testing.T) {
	const self = "created5"
	// The unexpected live branch: hh answers the fork-provoking edit by
	// changing the shared original in place. The dedup must revert the
	// record byte-for-byte and fail loudly, leaving no drift behind.
	pool := &[]string{
		dedupRecord(t, dedupSpec{100, "Компания А", []string{"base1", self}, "дрейф-мишень"}),
		dedupRecord(t, dedupSpec{110, "Компания А", []string{"clone1", self}, "новейший"}),
	}
	surface := &replaceSurface{nextID: 5000, reassignShared: true, inPlaceEdit: true, pool: *pool}
	env := newTestEnv(t)
	surface.mount(env, self)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	oldInterval, oldBudget := dedupPollInterval, dedupForkBudget
	dedupPollInterval, dedupForkBudget = 2*time.Millisecond, 5*time.Second
	defer func() { dedupPollInterval, dedupForkBudget = oldInterval, oldBudget }()
	ctx, cancel := ctxb(t)
	defer cancel()

	_, err := c.DetachDuplicateExperience(ctx, self)
	if !errors.Is(err, ErrExperienceInPlace) {
		t.Fatalf("err=%v, want ErrExperienceInPlace", err)
	}
	final := surface.snapshot()
	owners, desc := dedupMembership(t, final, 100)
	if desc != "дрейф-мишень" {
		t.Fatalf("record 100 desc=%q after revert, want original", desc)
	}
	found := false
	for _, o := range owners {
		if o == self {
			found = true
		}
	}
	if !found {
		t.Fatalf("record 100 lost self during in-place revert: %v", owners)
	}
}
