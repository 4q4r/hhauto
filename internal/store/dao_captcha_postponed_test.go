package store

import (
	"context"
	"testing"
	"time"
)

// T89: the captcha_postponed registry — park upsert (retry_at +
// retry_count increment on the same row), the due listing per account and
// resume, and the delete on settle.
func TestCaptchaPostponedRegistry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	park := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

	count, err := db.CaptchaPostponedPark(ctx, &CaptchaPostponed{
		AccountID: "main", ResumeHash: "h1", VacancyID: 7, RunID: "run1",
		RetryAt: park.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	if count != 1 {
		t.Fatalf("first park count = %d, want 1", count)
	}

	// Re-parking the same vacancy updates the row in place and bumps the
	// retry counter.
	count, err = db.CaptchaPostponedPark(ctx, &CaptchaPostponed{
		AccountID: "main", ResumeHash: "h1", VacancyID: 7, RunID: "run1",
		RetryAt: park.Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("re-park: %v", err)
	}
	if count != 2 {
		t.Fatalf("re-park count = %d, want 2", count)
	}

	// The due listing scopes to (account, resume) and honors retry_at —
	// the re-park moved the row to the 4h mark, so it is due exactly there.
	due, err := db.CaptchaPostponedDue(ctx, "main", "h1", park.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 || due[0].VacancyID != 7 || due[0].RetryCount != 2 {
		t.Fatalf("due = %+v, want vacancy 7 with count 2", due)
	}
	if !due[0].RetryAt.Equal(park.Add(4 * time.Hour)) {
		t.Fatalf("due retry_at = %v, want the latest park moment", due[0].RetryAt)
	}
	if early, err := db.CaptchaPostponedDue(ctx, "main", "h1", park.Add(2*time.Hour)); err != nil || len(early) != 0 {
		t.Fatalf("rows before retry_at = %+v, %v; want empty", early, err)
	}
	if other, err := db.CaptchaPostponedDue(ctx, "main", "h2", park.Add(2*time.Hour)); err != nil || len(other) != 0 {
		t.Fatalf("other resume due = %+v, %v; want empty", other, err)
	}
	if future, err := db.CaptchaPostponedDue(ctx, "main", "h1", park); err != nil || len(future) != 0 {
		t.Fatalf("not-yet-due rows = %+v, %v; want empty", future, err)
	}

	if err := db.CaptchaPostponedDelete(ctx, "main", "h1", 7); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if left, err := db.CaptchaPostponedDue(ctx, "main", "h1", park.Add(9*time.Hour)); err != nil || len(left) != 0 {
		t.Fatalf("rows after delete = %+v, %v; want empty", left, err)
	}
	if err := db.CaptchaPostponedDelete(ctx, "main", "h1", 7); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
}
