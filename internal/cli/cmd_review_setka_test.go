package cli

// T61 E2E: the approve-mode resolution flow over a parked setka_dm review
// item through the real cobra tree — discard marks the ledger row skipped
// and closes the item; apply without the captured endpoints fails loudly
// and the item stays open (the transport waits for live reconnaissance);
// a resolved item refuses a second resolve.

import (
	"fmt"
	"strings"
	"testing"

	"hhauto/internal/features/setka"
	"hhauto/internal/store"
)

// seedSetkaDMItem parks one open setka_dm review item with a bound ledger
// row and returns (review id, dm id).
func seedSetkaDMItem(t *testing.T, cfgPath string) (int64, int64) {
	t.Helper()
	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	dmID, err := rt.DB.SetkaDMCreate(newContext(), &store.SetkaDM{
		AccountID: "main", ApplicationDedupKey: "main:r1:hash1:7",
		VacancyID: 7, EmployerID: 42,
		PersonID: "u-hr-1", PersonName: "Мария", Company: "Acme",
		Mode: "approve", Status: "queued_review",
		Text: "Здравствуйте! Откликнулся на вашу вакансию Middle DevOps — готов рассказать подробнее.",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"dm_id":%d,"person_id":"u-hr-1","person_name":"Мария","company":"Acme",`+
		`"vacancy_id":7,"vacancy_title":"Middle DevOps-инженер","application":"main:r1:hash1:7",`+
		`"text":"Здравствуйте! Откликнулся на вашу вакансию Middle DevOps — готов рассказать подробнее."}`, dmID)
	id, err := rt.DB.ReviewItemsAdd(newContext(), "main", setka.ReviewKindSetkaDM,
		"setka: ЛС готово — требуется подтверждение отправки (режим approve)", payload)
	if err != nil {
		t.Fatal(err)
	}
	return id, dmID
}

func setkaDMRowStatus(t *testing.T, cfgPath string, dmID int64) (status, reviewBound string) {
	t.Helper()
	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	row, err := rt.DB.SetkaDMGet(newContext(), dmID)
	if err != nil || row == nil {
		t.Fatalf("row %d: %v", dmID, err)
	}
	if row.ReviewID != nil {
		return row.Status, fmt.Sprint(*row.ReviewID)
	}
	return row.Status, ""
}

// Discard closes the review, marks the ledger row skipped and sends nothing.
func TestReviewResolveSetkaDiscardE2E(t *testing.T) {
	cfgPath := writeTestConfig(t, "")
	id, dmID := seedSetkaDMItem(t, cfgPath)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resolve discard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "отклонено") {
		t.Fatalf("output = %s", out)
	}
	status, _ := setkaDMRowStatus(t, cfgPath, dmID)
	if status != "skipped" {
		t.Fatalf("row status = %s, want skipped", status)
	}
	if _, resolution := reviewItemByID(t, cfgPath, id); resolution == "" || !strings.Contains(resolution, "setka dm") {
		t.Fatalf("resolution = %q", resolution)
	}

	// A second resolve refuses.
	if _, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard", "--config", cfgPath); err == nil {
		t.Fatal("second resolve must fail")
	}
}

// Apply without the captured Setka endpoints fails loudly and the item
// stays open — never a silent skip.
func TestReviewResolveSetkaApplyWithoutEndpoints(t *testing.T) {
	cfgPath := writeTestConfig(t, "")
	id, dmID := seedSetkaDMItem(t, cfgPath)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "apply", "--config", cfgPath)
	if err == nil {
		t.Fatalf("apply must fail without endpoints, out: %s", out)
	}
	if !strings.Contains(err.Error(), "не захвачен") {
		t.Fatalf("err = %v, want the not-captured message", err)
	}
	status, _ := setkaDMRowStatus(t, cfgPath, dmID)
	if status != "queued_review" {
		t.Fatalf("row status = %s, want queued_review (nothing sent)", status)
	}
	if it, resolution := reviewItemByID(t, cfgPath, id); it != "open" || resolution != "" {
		t.Fatalf("item must stay open, got %s/%q", it, resolution)
	}
}

// Apply with --dry-run validates and reports, sends nothing, keeps the item
// open.
func TestReviewResolveSetkaApplyDryRun(t *testing.T) {
	cfgPath := writeTestConfig(t, "")
	id, dmID := seedSetkaDMItem(t, cfgPath)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "apply", "--dry-run", "--config", cfgPath)
	// The endpoints are missing → the client constructor fails before the
	// dry-run gate: still a loud configuration error, item stays open.
	if err == nil || !strings.Contains(err.Error(), "не захвачен") {
		t.Fatalf("dry-run with uncaptured endpoints must fail loudly: %v\n%s", err, out)
	}
	status, _ := setkaDMRowStatus(t, cfgPath, dmID)
	if status != "queued_review" {
		t.Fatalf("row status = %s, want queued_review", status)
	}
}
