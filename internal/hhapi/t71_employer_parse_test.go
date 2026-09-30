package hhapi

import (
	"encoding/json"
	"testing"
)

// T71: the SSR search item names the employer block "company" (live-probed
// 2026-09-30); the legacy "employer" shape still surfaces on some pages.
// A missing fallback parsed EmployerID=0 for every search vacancy, and the
// whole pool collapsed into one per-employer quota bucket (emp:0) — the
// 2026-09-30 daemon applied exactly 2 vacancies and capped the rest of the
// night (593 employer_daily_cap skips).
func TestVacancyFromItemCompanyEmployerBlock(t *testing.T) {
	raw := json.RawMessage(`{
		"id": 90599106,
		"name": "SRE / DevOps инженер",
		"company": {"id": 4890, "name": "Аккредитованная компания"},
		"area": {"name": "Москва"}
	}`)
	v := vacancyFromItem(raw)
	if v.EmployerID != 4890 || v.EmployerName != "Аккредитованная компания" {
		t.Fatalf("employer = (%d, %q), want (4890, Аккредитованная компания)", v.EmployerID, v.EmployerName)
	}
	if v.ID != 90599106 || v.Name != "SRE / DevOps инженер" {
		t.Fatalf("vacancy = (%d, %q)", v.ID, v.Name)
	}
}

// The legacy shape keeps parsing when "employer" is present.
func TestVacancyFromItemLegacyEmployerBlock(t *testing.T) {
	raw := json.RawMessage(`{
		"id": 95670838,
		"name": "DevOps",
		"employer": {"id": 1234, "name": "Legacy LLC"},
		"area": {"name": "Таганрог"}
	}`)
	v := vacancyFromItem(raw)
	if v.EmployerID != 1234 || v.EmployerName != "Legacy LLC" {
		t.Fatalf("employer = (%d, %q), want (1234, Legacy LLC)", v.EmployerID, v.EmployerName)
	}
}
