package store

import (
	"context"
	"testing"
	"time"
)

// TestSchedulesListAllAndSetEnabled covers the wave-3 mini-DAO additions:
// SchedulesListAll (full row read for engine restart preservation) and
// SchedulesSetEnabled (partial enable flip that must not touch
// last_run_at/next_run_at).
func TestSchedulesListAllAndSetEnabled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	last := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	next := last.Add(time.Hour)
	if err := db.SchedulesUpsert(ctx, &Schedule{
		ID: "s@a1", AccountID: "a1", Spec: `{"name":"s"}`, Enabled: true,
		LastRunAt: &last, NextRunAt: &next,
	}); err != nil {
		t.Fatalf("SchedulesUpsert: %v", err)
	}

	rows, err := db.SchedulesListAll(ctx)
	if err != nil {
		t.Fatalf("SchedulesListAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "s@a1" || r.AccountID != "a1" || !r.Enabled {
		t.Fatalf("row mismatch: %+v", r)
	}
	if r.LastRunAt == nil || !r.LastRunAt.Equal(last) {
		t.Errorf("last_run_at = %v, want %v", r.LastRunAt, last)
	}
	if r.NextRunAt == nil || !r.NextRunAt.Equal(next) {
		t.Errorf("next_run_at = %v, want %v (jitter preserved)", r.NextRunAt, next)
	}

	// SetEnabled flips only the enabled column.
	if err := db.SchedulesSetEnabled(ctx, "s@a1", false); err != nil {
		t.Fatalf("SchedulesSetEnabled(false): %v", err)
	}
	rows, _ = db.SchedulesListAll(ctx)
	if rows[0].Enabled {
		t.Fatal("row must be disabled")
	}
	if rows[0].NextRunAt == nil || !rows[0].NextRunAt.Equal(next) {
		t.Fatalf("disable must not touch next_run_at: got %v, want %v", rows[0].NextRunAt, next)
	}
	if rows[0].LastRunAt == nil || !rows[0].LastRunAt.Equal(last) {
		t.Fatalf("disable must not touch last_run_at: got %v, want %v", rows[0].LastRunAt, last)
	}

	// Re-enable, and a missing row is a no-op error-free? No: missing row
	// must be reported (the engine only calls it for known IDs, but the
	// DAO stays honest).
	if err := db.SchedulesSetEnabled(ctx, "s@a1", true); err != nil {
		t.Fatalf("SchedulesSetEnabled(true): %v", err)
	}
	if err := db.SchedulesSetEnabled(ctx, "missing@a1", true); err == nil {
		t.Fatal("SetEnabled on missing row must error")
	}
}
