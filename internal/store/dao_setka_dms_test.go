package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// T61: the setka_dms ledger — create, person-per-vacancy existence, day /
// company / person counters, status transitions.
func TestSetkaDMsLedger(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := now.Format("2006-01-02")

	id, err := db.SetkaDMCreate(ctx, &SetkaDM{
		AccountID: "main", ApplicationDedupKey: "main:r1:v7", VacancyID: 7, EmployerID: 42,
		PersonID: "u-1", PersonName: "Иван Иванов", Company: "Acme",
		Mode: "auto", Status: "queued_review", Text: "привет",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("id must be assigned")
	}

	// The same person on the same vacancy exists — the idempotency check.
	if ok, err := db.SetkaDMExists(ctx, "main", "u-1", 7); err != nil || !ok {
		t.Fatalf("exists = %v, %v; want true", ok, err)
	}
	if ok, _ := db.SetkaDMExists(ctx, "main", "u-1", 8); ok {
		t.Fatal("other vacancy must not match")
	}
	if ok, _ := db.SetkaDMExists(ctx, "other", "u-1", 7); ok {
		t.Fatal("other account must not match")
	}

	// DB-level backstop: a duplicate (account, person, vacancy) insert fails.
	if _, err := db.SetkaDMCreate(ctx, &SetkaDM{
		AccountID: "main", ApplicationDedupKey: "main:r2:v7", VacancyID: 7,
		PersonID: "u-1", Mode: "auto", Status: "queued_review", Text: "ещё раз",
	}); err == nil {
		t.Fatal("duplicate (account, person, vacancy) must be rejected")
	}

	// Counters start at zero (nothing sent yet) and count only sent rows.
	if n, _ := db.SetkaDMsCountDay(ctx, "main", day); n != 0 {
		t.Fatalf("day count before send = %d, want 0", n)
	}
	if err := db.SetkaDMMarkSent(ctx, id); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if n, _ := db.SetkaDMsCountDay(ctx, "main", day); n != 1 {
		t.Fatalf("day count = %d, want 1", n)
	}
	if n, _ := db.SetkaDMsCountDayByEmployer(ctx, "main", 42, day); n != 1 {
		t.Fatalf("employer count = %d, want 1", n)
	}
	if n, _ := db.SetkaDMsCountDayByPerson(ctx, "main", "u-1", day); n != 1 {
		t.Fatalf("person count = %d, want 1", n)
	}
	if n, _ := db.SetkaDMsCountDay(ctx, "main", "2000-01-01"); n != 0 {
		t.Fatal("another day must be 0")
	}

	// A skipped row never counts as a send.
	skID, err := db.SetkaDMCreate(ctx, &SetkaDM{
		AccountID: "main", ApplicationDedupKey: "main:r1:v9", VacancyID: 9, EmployerID: 42,
		PersonID: "u-2", Mode: "approve", Status: "queued_review", Text: "x",
	})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if err := db.SetkaDMMarkSkipped(ctx, skID, "review discard"); err != nil {
		t.Fatalf("mark skipped: %v", err)
	}
	if n, _ := db.SetkaDMsCountDay(ctx, "main", day); n != 1 {
		t.Fatalf("skipped row must not count: %d", n)
	}

	// Review binding for the approve flow.
	if err := db.SetkaDMSetReview(ctx, id, 77); err != nil {
		t.Fatalf("set review: %v", err)
	}
	dm, err := db.SetkaDMGet(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dm.ReviewID == nil || *dm.ReviewID != 77 {
		t.Fatalf("review id = %v, want 77", dm.ReviewID)
	}
	if dm.Status != "sent" || dm.SentAt == nil {
		t.Fatalf("status = %s, sent_at = %v", dm.Status, dm.SentAt)
	}
	if dm.CreatedAt.IsZero() {
		t.Fatal("created_at must be filled")
	}
}

// T61: unknown id → nil, no error.
func TestSetkaDMGetUnknown(t *testing.T) {
	db := openTestDB(t)
	dm, err := db.SetkaDMGet(context.Background(), 99999)
	if err != nil || dm != nil {
		t.Fatalf("want nil,nil, got %v,%v", dm, err)
	}
	_ = errors.Is // keep errors import if assertions evolve
}
