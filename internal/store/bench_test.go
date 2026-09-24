package store

// Benchmarks for the write-path DAOs the apply funnel hits per vacancy:
// ApplicationsCreate (INSERT with UNIQUE dedup), ApplicationsCountToday
// (quota read over an indexed table), IntentsBegin (idempotency claim).

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func benchDB(b *testing.B) *DB {
	b.Helper()
	db, err := Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"), Options{BusyTimeoutMS: 2000})
	if err != nil {
		b.Fatalf("store.Open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

func BenchmarkApplicationsCreate(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()
	n := 0
	b.ReportAllocs()
	for b.Loop() {
		n++
		a := &Application{
			AccountID: "acc", RunID: "run", HhResumeID: "hash1",
			VacancyID: int64(n), EmployerID: 7,
			DedupKey: fmt.Sprintf("acc|hash1|%d", n),
			Status:   "applied",
		}
		if err := db.ApplicationsCreate(ctx, a); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplicationsCountToday(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()
	// Seed 100 applied rows "today" so the COUNT scans a realistic day.
	applied := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= 100; i++ {
		a := &Application{
			AccountID: "acc", RunID: "run", HhResumeID: "hash1",
			VacancyID: int64(i), EmployerID: int64(100 + i),
			DedupKey: fmt.Sprintf("acc|hash1|%d", i),
			Status:   "applied", AppliedAt: &applied,
		}
		if err := db.ApplicationsCreate(ctx, a); err != nil {
			b.Fatal(err)
		}
	}
	day := applied.Format("2006-01-02")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := db.ApplicationsCountToday(ctx, "acc", "run", day); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIntentsBegin(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()
	n := 0
	b.ReportAllocs()
	for b.Loop() {
		n++
		// Fresh key per op = the insert path; re-arming a failed intent
		// (the UPDATE path) is measured by the queue/feature suites.
		if _, err := db.IntentsBegin(ctx, fmt.Sprintf("apply:acc|hash1|%d", n), "pending", "{}"); err != nil {
			b.Fatal(err)
		}
	}
}
