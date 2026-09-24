package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMetricKeyConstants(t *testing.T) {
	// The metric keys are an operator-facing contract (dashboards, greps
	// over the DB): pin them.
	want := map[string]string{
		KeyApplicationsSent: "applications_sent",
		KeyHTTP429:          "http_429",
		KeyCaptchasSolved:   "captchas_solved",
		KeyAITokensIn:       "ai_tokens_in",
		KeyAITokensOut:      "ai_tokens_out",
		KeyPingsSent:        "pings_sent",
		KeyEmailsSent:       "emails_sent",
		KeyTasksFailed:      "tasks_failed",
	}
	for k, v := range want {
		if k != v {
			t.Errorf("key constant %q must keep its stable value %q", k, v)
		}
	}
}

func TestIncrAndSnapshot(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Incr(ctx, db, KeyApplicationsSent); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if err := Incr(ctx, db, KeyApplicationsSent); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if err := Incr(ctx, db, KeyVacanciesScanned); err != nil {
		t.Fatalf("Incr: %v", err)
	}

	f, err := Snapshot(ctx, db, Day())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if f.Applied != 2 {
		t.Errorf("Applied = %d, want 2", f.Applied)
	}
	if f.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", f.Scanned)
	}
	if f.SkippedByStage == nil || len(f.SkippedByStage) != 0 {
		t.Errorf("SkippedByStage must be a non-nil empty map, got %v", f.SkippedByStage)
	}
}

func TestSnapshotDayIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Incr(ctx, db, KeyVacanciesScanned); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	yesterday := DayAt(time.Now().UTC().AddDate(0, 0, -1))
	f, err := Snapshot(ctx, db, yesterday)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if f.Scanned != 0 {
		t.Errorf("yesterday Scanned = %d, want 0 (day isolation)", f.Scanned)
	}
}

func TestSnapshotSkippedByStage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, key := range []string{"skipped_low_score", "skipped_agency", "skipped_low_score"} {
		if err := Incr(ctx, db, key); err != nil {
			t.Fatalf("Incr(%s): %v", key, err)
		}
	}
	f, err := Snapshot(ctx, db, Day())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if f.SkippedByStage["low_score"] != 2 {
		t.Errorf("skipped low_score = %d, want 2", f.SkippedByStage["low_score"])
	}
	if f.SkippedByStage["agency"] != 1 {
		t.Errorf("skipped agency = %d, want 1", f.SkippedByStage["agency"])
	}
	// The raw skipped_* keys must not leak into the plain funnel numbers.
	if f.Scanned != 0 || f.Passed != 0 || f.Applied != 0 {
		t.Errorf("skipped counters leaked into funnel: %+v", f)
	}
}

func TestDayFormat(t *testing.T) {
	day := DayAt(time.Date(2026, 9, 24, 23, 30, 0, 0, time.UTC))
	if day != "2026-09-24" {
		t.Errorf("DayAt = %q, want 2026-09-24", day)
	}
}
