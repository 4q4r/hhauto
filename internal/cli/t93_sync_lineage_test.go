package cli

// T93: the sync-lineage seed — edges register through the store API,
// idempotently, with the parent-derived depth; conflicts (an existing row
// naming a different parent) are reported, never rewritten (lineage is
// immutable once recorded).

import (
	"context"
	"path/filepath"
	"testing"

	"hhauto/internal/features/autoclone"
	"hhauto/internal/store"
)

func t93Store(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t93.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.AccountsUpsert(context.Background(), store.Account{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSyncLineageEdgesRegistersIdempotently(t *testing.T) {
	db := t93Store(t)
	ctx := context.Background()
	edges := []autoclone.LineageEdge{
		{Clone: "ccccaa11", Parent: "bbbb2233"},
		{Clone: "dddd44ee", Parent: "ccccaa11"}, // clone of a clone → depth 2
	}
	reg, tracked, conflicts, err := syncLineageEdges(ctx, db, "main", edges, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if reg != 2 || tracked != 0 || conflicts != 0 {
		t.Fatalf("first pass = %d/%d/%d, want 2/0/0", reg, tracked, conflicts)
	}
	row, err := db.ResumeClonesGet(ctx, "dddd44ee")
	if err != nil || row == nil {
		t.Fatalf("clone row: %v %v", row, err)
	}
	if row.ParentHash != "ccccaa11" || row.Depth != 2 || row.AccountID != "main" {
		t.Fatalf("row = %+v", row)
	}
	// Idempotent: the second pass registers nothing new.
	reg, tracked, conflicts, err = syncLineageEdges(ctx, db, "main", edges, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if reg != 0 || tracked != 2 || conflicts != 0 {
		t.Fatalf("second pass = %d/%d/%d, want 0/2/0", reg, tracked, conflicts)
	}
	// A conflicting row (different parent) is reported, not rewritten.
	if err := db.ResumeClonesRegister(ctx, &store.ResumeClone{
		CloneHash: "eeee55ff", ParentHash: "other9999", Depth: 1, AccountID: "main",
	}); err != nil {
		t.Fatal(err)
	}
	reg, tracked, conflicts, err = syncLineageEdges(ctx, db, "main", []autoclone.LineageEdge{
		{Clone: "eeee55ff", Parent: "bbbb2233"},
	}, false)
	if err != nil {
		t.Fatalf("conflict sync: %v", err)
	}
	if reg != 0 || tracked != 0 || conflicts != 1 {
		t.Fatalf("conflict pass = %d/%d/%d, want 0/0/1", reg, tracked, conflicts)
	}
	if row, _ := db.ResumeClonesGet(ctx, "eeee55ff"); row == nil || row.ParentHash != "other9999" {
		t.Fatalf("conflicting row was rewritten: %+v", row)
	}
}

// Dry mode reports the same numbers but writes nothing.
func TestSyncLineageEdgesDryWritesNothing(t *testing.T) {
	db := t93Store(t)
	reg, tracked, conflicts, err := syncLineageEdges(context.Background(), db, "main",
		[]autoclone.LineageEdge{{Clone: "ccccaa11", Parent: "bbbb2233"}}, true)
	if err != nil {
		t.Fatalf("dry sync: %v", err)
	}
	if reg != 1 || tracked != 0 || conflicts != 0 {
		t.Fatalf("dry pass = %d/%d/%d, want 1/0/0", reg, tracked, conflicts)
	}
	if row, _ := db.ResumeClonesGet(context.Background(), "ccccaa11"); row != nil {
		t.Fatalf("dry run wrote a row: %+v", row)
	}
}
