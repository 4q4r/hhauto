package store

import (
	"context"
	"testing"
	"time"
)

// T61: ApplicationsAppliedDue — the setka scan source: status='applied',
// applied_at in [from, to], newest first, other accounts isolated.
func TestApplicationsAppliedDue(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(dedup string, vac int64, appliedAt time.Time) {
		a := &Application{
			AccountID: "main", RunID: "r", HhResumeID: "h", VacancyID: vac, EmployerID: 42,
			DedupKey: dedup, Status: "applied", AppliedAt: &appliedAt,
		}
		if err := db.ApplicationsCreate(ctx, a); err != nil {
			t.Fatalf("create %s: %v", dedup, err)
		}
	}
	mk("k1", 1, now.Add(-2*time.Hour))    // due
	mk("k2", 2, now.Add(-30*time.Minute)) // not yet due
	mk("k3", 3, now.Add(-96*time.Hour))   // outside lookback
	other := &Application{
		AccountID: "other", VacancyID: 4, EmployerID: 1, DedupKey: "k4", Status: "applied",
		AppliedAt: &now,
	}
	if err := db.ApplicationsCreate(ctx, other); err != nil {
		t.Fatal(err)
	}
	pending := &Application{AccountID: "main", VacancyID: 5, EmployerID: 1, DedupKey: "k5", Status: "queued"}
	if err := db.ApplicationsCreate(ctx, pending); err != nil {
		t.Fatal(err)
	}

	due, err := db.ApplicationsAppliedDue(ctx, "main", now.Add(-72*time.Hour), now.Add(-1*time.Hour), 50)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 || due[0].DedupKey != "k1" {
		t.Fatalf("due = %+v, want [k1]", due)
	}
}
