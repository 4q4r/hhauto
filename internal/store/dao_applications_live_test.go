package store

import "testing"

// ApplicationsGetByDedupKeyLive must ignore dry_run survey rows: they must
// never block a real application (Wave 3 review fix for T10).
func TestApplicationsGetByDedupKeyLiveIgnoresDryRun(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	err := db.ApplicationsCreate(ctx, &Application{
		AccountID: "a", RunID: "r", HhResumeID: "h",
		VacancyID: 1, EmployerID: 2, DedupKey: "k1", Status: "dry_run",
	})
	if err != nil {
		t.Fatalf("create dry_run: %v", err)
	}

	got, err := db.ApplicationsGetByDedupKeyLive(ctx, "k1")
	if err != nil || got != nil {
		t.Fatalf("live lookup = %+v err=%v, want nil (dry_run ignored)", got, err)
	}

	// A non-dry_run record is returned.
	if err := db.ApplicationsCreate(ctx, &Application{
		AccountID: "a", RunID: "r", HhResumeID: "h",
		VacancyID: 2, EmployerID: 2, DedupKey: "k2", Status: "applied",
	}); err != nil {
		t.Fatalf("create applied: %v", err)
	}
	got, err = db.ApplicationsGetByDedupKeyLive(ctx, "k2")
	if err != nil || got == nil || got.Status != "applied" {
		t.Fatalf("live lookup = %+v err=%v, want applied row", got, err)
	}

	// Missing keys stay nil.
	got, err = db.ApplicationsGetByDedupKeyLive(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("missing = %+v err=%v, want nil", got, err)
	}
}
