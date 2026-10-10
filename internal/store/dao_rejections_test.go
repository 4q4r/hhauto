package store

import (
	"context"
	"testing"
	"time"
)

// T123: the rejections registry — one analyzed rejection per
// (account, vacancy); a re-analysis replaces the row in place
// (idempotency), the list is scoped to the account, newest rejection first.
func TestRejectionsRegistry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	stamp := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return stamp }

	rejected := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	put := func(rootCause string, evidence bool) error {
		return db.RejectionsPut(ctx, &Rejection{
			AccountID: "main", VacancyID: 1001, VacancyName: "DevOps-инженер",
			EmployerName: "Acme", NegotiationID: "9001", RejectedAt: rejected,
			RootCause: rootCause, EvidenceBased: evidence,
			ImprovedLetter: "Улучшенное письмо", MissingSkills: []string{"Kubernetes", "Terraform"},
			Takeaway: "Добавить Kubernetes в key_skills",
		})
	}
	if err := put("Просят Kubernetes, в резюме его нет", true); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Re-analysis of the same vacancy updates the row in place: still one
	// row, the newest analysis wins.
	if err := put("Другая причина после переанализа", false); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	row, err := db.RejectionsGet(ctx, "main", 1001)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row == nil {
		t.Fatal("row is nil after upsert")
	}
	if row.RootCause != "Другая причина после переанализа" || row.EvidenceBased {
		t.Fatalf("row = %+v, want the re-analysis verdict", row)
	}
	if len(row.MissingSkills) != 2 || row.MissingSkills[0] != "Kubernetes" {
		t.Fatalf("missing_skills = %+v", row.MissingSkills)
	}
	if !row.RejectedAt.Equal(rejected) {
		t.Fatalf("rejected_at = %v, want %v", row.RejectedAt, rejected)
	}
	if !row.CreatedAt.Equal(stamp) {
		t.Fatalf("created_at = %v, want the store clock %v", row.CreatedAt, stamp)
	}

	// Absent row: nil, no error — the analyzer's skip gate.
	if got, gerr := db.RejectionsGet(ctx, "main", 404); gerr != nil || got != nil {
		t.Fatalf("get absent = %+v, %v; want nil, nil", got, gerr)
	}

	// The list is per-account, newest rejection first.
	older := rejected.AddDate(0, 0, -3)
	if err := db.RejectionsPut(ctx, &Rejection{
		AccountID: "main", VacancyID: 1002, VacancyName: "Backend",
		EmployerName: "Beta", RejectedAt: older,
		RootCause: "Не сошлись по зарплате", EvidenceBased: true,
	}); err != nil {
		t.Fatalf("put 1002: %v", err)
	}
	rows, err := db.RejectionsList(ctx, "main")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].VacancyID != 1001 || rows[1].VacancyID != 1002 {
		t.Fatalf("order = %d,%d; want newest (1001) first", rows[0].VacancyID, rows[1].VacancyID)
	}

	// Other accounts do not see these rows.
	other, err := db.RejectionsList(ctx, "second")
	if err != nil || len(other) != 0 {
		t.Fatalf("other account rows = %+v, %v; want empty", other, err)
	}
}
