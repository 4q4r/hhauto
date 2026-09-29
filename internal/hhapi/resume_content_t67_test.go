package hhapi

// T67 experience verify/dedup tests, built on the live shape of clone
// 79d39537… (dump 2026-09-29, /tmp/opencode/t67/, de-personalized): the
// experience pool is the shared lineage store, and the first improve of a
// fresh clone edits records SHARED with the base resumes. hh accepts the
// save but materializes the answer fork asynchronously — in the live
// failing run it surfaced only after the verify budget had expired (the
// dump shows the fork carrying the sent content verbatim). The replace
// step must also never classify a shared original (it renders on the base
// resumes) as a deletable duplicate.
//
// The fixture keeps the full server record shape of the dump
// (companyAreaId/companyIndustries/verification/…), sanitized.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// t67Record renders one pool record in the full live dump shape; owners
// lists the hashes the record renders on.
func t67Record(id int64, desc string, owners ...string) string {
	rs := make([]string, 0, len(owners))
	for _, h := range owners {
		rs = append(rs, `{"hash":"`+h+`"}`)
	}
	return `{"id":` + itoa64(id) +
		`,"startDate":"2024-01-01","endDate":null` +
		`,"companyAreaId":null,"companyId":null,"companyIndustries":[],"companyIndustryId":null` +
		`,"companyLogos":[],"companyState":null,"companyUrl":null,"employerId":null,"industries":[]` +
		`,"companyName":"Компания T67","position":"Инженер T67"` +
		`,"professionId":43,"professionName":"Backend-разработчик"` +
		`,"verification":{"status":"NOT_VERIFIED","canModify":true}` +
		`,"resumes":[` + strings.Join(rs, ",") + `]` +
		`,"description":` + mustJSON(desc) + `}`
}

// t67ShrinkVerify collapses the verify budgets so a RED run fails fast.
func t67ShrinkVerify(t *testing.T) {
	t.Helper()
	oldInterval, oldBudget := verifyPollInterval, verifyWaitBudget
	oldForkBudget := forkVerifyWaitBudget
	verifyPollInterval, verifyWaitBudget, forkVerifyWaitBudget = 10*time.Millisecond, 60*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() {
		verifyPollInterval, verifyWaitBudget = oldInterval, oldBudget
		forkVerifyWaitBudget = oldForkBudget
	})
}

// t67EditDescription mutates one record's description in the split.
func t67EditDescription(t *testing.T, rc *ResumeContent, id int64, desc string) {
	t.Helper()
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(rc.Profile["experience"], &items); err != nil {
		t.Fatalf("decode experience: %v", err)
	}
	for i := range items {
		var got int64
		if json.Unmarshal(items[i]["id"], &got) == nil && got == id {
			items[i]["description"] = json.RawMessage(mustJSON(desc))
			b, err := json.Marshal(items)
			if err != nil {
				t.Fatalf("encode experience: %v", err)
			}
			rc.Profile["experience"] = b
			return
		}
	}
	t.Fatalf("record id=%d not in the split", id)
}

const (
	t67Clone = "cl0ne67a"
	t67Base1 = "b4se67b1"
	t67Base2 = "b4se67b2"
	t67Base3 = "b4se67b3"
)

// The failing live shape: the fork of the edited shared record materializes
// hh-side AFTER the ordinary verify budget (the dump proves it landed with
// the sent content verbatim). The fork-aware verify must wait on the
// experience fork budget, succeed once the carrier exists, and never touch
// the shared original.
func TestUpdateResumeContentVerifyWaitsForSharedRecordFork(t *testing.T) {
	t67ShrinkVerify(t)
	const sharedID = int64(2103588721) // the live dump's shared base record id
	surface := &replaceSurface{nextID: 3000, forkAfterReads: 12, reassignShared: true, pool: []string{
		t67Record(sharedID, "Старое описание", t67Base1, t67Base2, t67Base3, t67Clone),
	}}
	env := newTestEnv(t)
	surface.mount(env, t67Clone)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, t67Clone)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t67EditDescription(t, content, sharedID, "Новое описание (форк носитель)")
	if err := c.UpdateResumeContent(ctx, t67Clone, content, []string{"profile.experience"}); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}

	pool := surface.snapshot()
	if len(pool) != 2 {
		t.Fatalf("pool has %d records, want 2 (original + fork)", len(pool))
	}
	if t58PoolIndex(pool, sharedID) < 0 {
		t.Fatal("shared original vanished from the pool")
	}
	forkFound := false
	for _, raw := range pool {
		var rec struct {
			ID          int64  `json:"id"`
			Description string `json:"description"`
		}
		if json.Unmarshal([]byte(raw), &rec) != nil || rec.ID == sharedID {
			continue
		}
		forkFound = true
		if rec.Description != "Новое описание (форк носитель)" {
			t.Fatalf("fork description = %q", rec.Description)
		}
	}
	if !forkFound {
		t.Fatal("no fork carrier in the pool")
	}
	if surface.updatePosts != 0 {
		t.Fatalf("shared original must never be deleted (profile/update posts = %d)", surface.updatePosts)
	}
}

// The shared original keeps rendering on self when the replace step runs
// (reassignment lag): the flow must skip it (the T56 guard would refuse it
// anyway), delete only the genuinely replaced own record, and the final
// invariant must hold with the shared record still own.
func TestUpdateResumeContentReplaceSkipsSharedRecords(t *testing.T) {
	t67ShrinkVerify(t)
	const sharedID = int64(2103588721)
	const ownID = int64(2111066447)
	surface := &replaceSurface{nextID: 3100, pool: []string{
		t67Record(sharedID, "Старое общее описание", t67Base1, t67Base2, t67Base3, t67Clone),
		t58Record(ownID, "Старое своё описание", t67Clone),
	}}
	env := newTestEnv(t)
	surface.mount(env, t67Clone)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, t67Clone)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t67EditDescription(t, content, sharedID, "Новое общее описание")
	t67EditDescription(t, content, ownID, "Новое своё описание")
	if err := c.UpdateResumeContent(ctx, t67Clone, content, []string{"profile.experience"}); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}

	pool := surface.snapshot()
	if t58PoolIndex(pool, sharedID) < 0 {
		t.Fatal("shared original must stay in the pool (it renders on the bases)")
	}
	if t58PoolIndex(pool, ownID) >= 0 {
		t.Fatal("replaced own original must be deleted")
	}
	if surface.updatePosts != 1 {
		t.Fatalf("exactly one delete POST expected, got %d", surface.updatePosts)
	}
}

// The genuine silent no-op (T55 failure mode) must keep failing loudly —
// a longer fork budget must not paper over a save hh never applied.
func TestUpdateResumeContentExperienceNoopStillFails(t *testing.T) {
	t67ShrinkVerify(t)
	const sharedID = int64(2103588721)
	surface := &replaceSurface{nextID: 3200, silentEdit: true, pool: []string{
		t67Record(sharedID, "Старое описание", t67Base1, t67Base2, t67Base3, t67Clone),
	}}
	env := newTestEnv(t)
	surface.mount(env, t67Clone)
	c := replaceEndpoints(env, WithExperienceReplace(true))
	ctx, cancel := ctxb(t)
	defer cancel()

	live, err := c.ResumeContent(ctx, t67Clone)
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	content := live.Clone()
	t67EditDescription(t, content, sharedID, "Новое описание, которое hh проигнорировал")
	err = c.UpdateResumeContent(ctx, t67Clone, content, []string{"profile.experience"})
	var ve *ResumeContentVerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("want ResumeContentVerifyError, got %v", err)
	}
	if len(ve.Keys) != 1 || ve.Keys[0] != "profile.experience" {
		t.Fatalf("verify error keys = %v", ve.Keys)
	}
}
