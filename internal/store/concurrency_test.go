package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWriters proves the single-writer mutex serializes writes
// (no SQLITE_BUSY errors, exact totals) and that concurrent readers are
// race-free against writers. Run under: go test -race.
func TestConcurrentWriters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const writers = 8
	const perWriter = 25
	day := time.Now().UTC().Format("2006-01-02")

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter*2)

	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				app := Application{
					AccountID: fmt.Sprintf("acc-%d", g), RunID: "run-1",
					VacancyID: int64(i), EmployerID: int64(g),
					DedupKey: fmt.Sprintf("dk-%d-%d", g, i), Status: "queued",
				}
				if err := db.ApplicationsCreate(ctx, &app); err != nil {
					errs <- fmt.Errorf("writer %d: ApplicationsCreate: %w", g, err)
					continue
				}
				if err := db.ApplicationsMarkStatus(ctx, app.ID, "applied", nil); err != nil {
					errs <- fmt.Errorf("writer %d: MarkStatus: %w", g, err)
				}
				if err := db.MetricsIncr(ctx, "created", day); err != nil {
					errs <- fmt.Errorf("writer %d: MetricsIncr: %w", g, err)
				}
			}
		}(g)
	}

	// Concurrent readers exercising the non-mutexed read path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := db.ApplicationsCountToday(ctx, "acc-1", "run-1", day); err != nil {
				errs <- fmt.Errorf("reader: ApplicationsCountToday: %w", err)
			}
			if _, err := db.MetricsSnapshot(ctx, day); err != nil {
				errs <- fmt.Errorf("reader: MetricsSnapshot: %w", err)
			}
		}
	}()

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	snap, err := db.MetricsSnapshot(ctx, day)
	if err != nil {
		t.Fatalf("final MetricsSnapshot: %v", err)
	}
	if snap["created"] != writers*perWriter {
		t.Fatalf("metrics created = %d, want %d (lost updates?)", snap["created"], writers*perWriter)
	}

	total := 0
	for g := 0; g < writers; g++ {
		n, err := db.ApplicationsCountToday(ctx, fmt.Sprintf("acc-%d", g), "run-1", day)
		if err != nil {
			t.Fatalf("count acc-%d: %v", g, err)
		}
		if n != perWriter {
			t.Fatalf("acc-%d applications = %d, want %d", g, n, perWriter)
		}
		total += n
	}
	if total != writers*perWriter {
		t.Fatalf("total applications = %d, want %d", total, writers*perWriter)
	}
}
