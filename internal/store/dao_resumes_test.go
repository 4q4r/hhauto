package store

// T106: the store mirrors the live shard. ResumesPruneAbsent removes rows
// whose hash is absent from a successful full fetch (deleted-on-hh
// zombies), scoped to one account, touching nothing else.

import (
	"context"
	"testing"
)

func seedT106Resume(t *testing.T, db *DB, accountID, hash string) {
	t.Helper()
	r := Resume{AccountID: accountID, HhResumeID: hash, Title: "т " + hash, Status: "published", Raw: `{}`}
	if err := db.ResumesUpsert(context.Background(), &r); err != nil {
		t.Fatalf("seed resume %s/%s: %v", accountID, hash, err)
	}
}

func TestResumesPruneAbsentDeletesOnlyAbsentRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := db.AccountsUpsert(ctx, Account{ID: id}); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	seedT106Resume(t, db, "acc-1", "keep-1")
	seedT106Resume(t, db, "acc-1", "keep-2")
	seedT106Resume(t, db, "acc-1", "zombie-1")
	seedT106Resume(t, db, "acc-2", "zombie-1") // same hash, other account

	n, err := db.ResumesPruneAbsent(ctx, "acc-1", []string{"keep-1", "keep-2"})
	if err != nil {
		t.Fatalf("ResumesPruneAbsent: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	got, err := db.ResumesListByAccount(ctx, "acc-1")
	if err != nil {
		t.Fatalf("list acc-1: %v", err)
	}
	if len(got) != 2 || got[0].HhResumeID != "keep-1" || got[1].HhResumeID != "keep-2" {
		t.Fatalf("acc-1 rows = %v, want [keep-1 keep-2]", hashesOfT106(got))
	}
	other, err := db.ResumesListByAccount(ctx, "acc-2")
	if err != nil {
		t.Fatalf("list acc-2: %v", err)
	}
	if len(other) != 1 || other[0].HhResumeID != "zombie-1" {
		t.Fatalf("acc-2 rows = %v, prune must stay account-scoped", hashesOfT106(other))
	}
}

func TestResumesPruneAbsentEmptyKeepClearsAccount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := db.AccountsUpsert(ctx, Account{ID: id}); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	seedT106Resume(t, db, "acc-1", "gone-1")
	seedT106Resume(t, db, "acc-1", "gone-2")
	seedT106Resume(t, db, "acc-2", "stays")

	// An empty live shard mirrors to an empty store for that account
	// only (hh accounts may legitimately hold zero resumes).
	n, err := db.ResumesPruneAbsent(ctx, "acc-1", nil)
	if err != nil {
		t.Fatalf("ResumesPruneAbsent: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned = %d, want 2", n)
	}
	got, err := db.ResumesListByAccount(ctx, "acc-1")
	if err != nil {
		t.Fatalf("list acc-1: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("acc-1 rows = %v, want empty", hashesOfT106(got))
	}
	other, err := db.ResumesListByAccount(ctx, "acc-2")
	if err != nil {
		t.Fatalf("list acc-2: %v", err)
	}
	if len(other) != 1 || other[0].HhResumeID != "stays" {
		t.Fatalf("acc-2 rows = %v, want untouched", hashesOfT106(other))
	}
}

func hashesOfT106(rows []Resume) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.HhResumeID)
	}
	return out
}
