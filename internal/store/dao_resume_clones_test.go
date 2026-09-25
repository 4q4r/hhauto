package store

import (
	"context"
	"testing"
	"time"
)

// T26: resume_clones lineage (migration 0003) — hh does not expose the clone
// parent (parentResumeId is absent on live clones), so hhauto tracks lineage
// locally.

func TestResumeClonesRegisterAndGet(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	created := truncSec(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	clone := &ResumeClone{
		CloneHash:  "clonehash1",
		ParentHash: "parenthash1",
		Depth:      1,
		AccountID:  "acc1",
		CreatedAt:  created,
	}
	if err := db.ResumeClonesRegister(ctx, clone); err != nil {
		t.Fatalf("ResumeClonesRegister: %v", err)
	}

	got, err := db.ResumeClonesGet(ctx, "clonehash1")
	if err != nil {
		t.Fatalf("ResumeClonesGet: %v", err)
	}
	if got == nil {
		t.Fatalf("ResumeClonesGet = nil, want row")
	}
	if got.ParentHash != "parenthash1" || got.Depth != 1 || got.AccountID != "acc1" {
		t.Fatalf("row = %+v, want parent=parenthash1 depth=1 account=acc1", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}

	// Idempotent: re-registering the same clone hash must not duplicate or
	// rewrite lineage (identity is immutable once recorded).
	dup := &ResumeClone{CloneHash: "clonehash1", ParentHash: "OTHER", Depth: 9, AccountID: "acc2"}
	if err := db.ResumeClonesRegister(ctx, dup); err != nil {
		t.Fatalf("ResumeClonesRegister duplicate: %v", err)
	}
	again, err := db.ResumeClonesGet(ctx, "clonehash1")
	if err != nil {
		t.Fatalf("ResumeClonesGet after duplicate: %v", err)
	}
	if again.ParentHash != "parenthash1" || again.Depth != 1 || again.AccountID != "acc1" {
		t.Fatalf("duplicate rewrote lineage: %+v", again)
	}
}

func TestResumeClonesGetMissing(t *testing.T) {
	db := openTestDB(t)
	got, err := db.ResumeClonesGet(context.Background(), "nope")
	if err != nil {
		t.Fatalf("ResumeClonesGet: %v", err)
	}
	if got != nil {
		t.Fatalf("ResumeClonesGet = %+v, want nil for unknown hash", got)
	}
}

func TestResumeClonesListAndCountChildren(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	rows := []ResumeClone{
		{CloneHash: "c1", ParentHash: "p1", Depth: 1, AccountID: "acc1"},
		{CloneHash: "c2", ParentHash: "p1", Depth: 1, AccountID: "acc1"},
		{CloneHash: "c3", ParentHash: "c1", Depth: 2, AccountID: "acc1"},
		{CloneHash: "cx", ParentHash: "px", Depth: 1, AccountID: "acc2"},
	}
	for i := range rows {
		if err := db.ResumeClonesRegister(ctx, &rows[i]); err != nil {
			t.Fatalf("ResumeClonesRegister %s: %v", rows[i].CloneHash, err)
		}
	}

	got, err := db.ResumeClonesListByAccount(ctx, "acc1")
	if err != nil {
		t.Fatalf("ResumeClonesListByAccount: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("list len = %d, want 3 (acc1 rows only)", len(got))
	}

	n, err := db.ResumeClonesCountChildren(ctx, "acc1", "p1")
	if err != nil {
		t.Fatalf("ResumeClonesCountChildren: %v", err)
	}
	if n != 2 {
		t.Fatalf("children of p1 = %d, want 2", n)
	}
	n, err = db.ResumeClonesCountChildren(ctx, "acc1", "c1")
	if err != nil {
		t.Fatalf("ResumeClonesCountChildren: %v", err)
	}
	if n != 1 {
		t.Fatalf("children of c1 = %d, want 1", n)
	}
}

func TestChatsListRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	recent := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	old := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	rows := []Chat{
		{AccountID: "acc1", ChatikChatID: "chat1", State: "rejected", LastMessageAt: &recent, UpdatedAt: recent},
		{AccountID: "acc1", ChatikChatID: "chat2", State: "rejected", LastMessageAt: &old, UpdatedAt: old},
		{AccountID: "acc1", ChatikChatID: "chat3", State: "active", LastMessageAt: &recent, UpdatedAt: recent},
		{AccountID: "acc2", ChatikChatID: "chat4", State: "rejected", LastMessageAt: &recent, UpdatedAt: recent},
	}
	for i := range rows {
		if err := db.ChatsUpsert(ctx, &rows[i]); err != nil {
			t.Fatalf("ChatsUpsert %s: %v", rows[i].ChatikChatID, err)
		}
	}

	got, err := db.ChatsListRejected(ctx, "acc1")
	if err != nil {
		t.Fatalf("ChatsListRejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rejected chats = %d, want 2 (state and account filtered)", len(got))
	}
	for _, c := range got {
		if c.VacancyID != nil {
			t.Fatalf("vacancy filter mismatch: chat %s carries vacancy", c.ChatikChatID)
		}
	}
	if got[0].LastMessageAt == nil || !got[0].LastMessageAt.Equal(recent) {
		t.Fatalf("newest-first ordering broken: %+v", got[0].LastMessageAt)
	}
}

func TestApplicationsListByAccountAndVacancies(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	apps := []Application{
		{AccountID: "acc1", HhResumeID: "p1", VacancyID: 100, DedupKey: "k1", Status: "applied"},
		{AccountID: "acc1", HhResumeID: "c1", VacancyID: 100, DedupKey: "k2", Status: "applied"},
		{AccountID: "acc1", HhResumeID: "p1", VacancyID: 200, DedupKey: "k3", Status: "failed"},
		{AccountID: "acc2", HhResumeID: "p1", VacancyID: 100, DedupKey: "k4", Status: "applied"},
	}
	for i := range apps {
		if err := db.ApplicationsCreate(ctx, &apps[i]); err != nil {
			t.Fatalf("ApplicationsCreate %d: %v", i, err)
		}
	}

	got, err := db.ApplicationsListByAccountAndVacancies(ctx, "acc1", []int64{100, 200})
	if err != nil {
		t.Fatalf("ApplicationsListByAccountAndVacancies: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (account filtered)", len(got))
	}

	got, err = db.ApplicationsListByAccountAndVacancies(ctx, "acc1", []int64{100})
	if err != nil {
		t.Fatalf("ApplicationsListByAccountAndVacancies: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}

	// Empty id list: defined empty result, never a full-table scan.
	got, err = db.ApplicationsListByAccountAndVacancies(ctx, "acc1", nil)
	if err != nil {
		t.Fatalf("empty ids: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty ids = %d rows, want 0", len(got))
	}
}
