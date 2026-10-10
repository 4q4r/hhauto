package hhapi

// T115: the search item carries the pre-filter inputs userLabels and
// totalResponsesCount for free (donor R item fields, live fixture shape
// from the T70 capture); vacancyFromItem must surface them typed so the
// multiapply pre-filters can skip before the popup/card.

import (
	"encoding/json"
	"testing"
)

// T115 golden: the depersonalized live search item (search_item_badges.json,
// captured with both fields present) parses totalResponsesCount and the
// empty userLabels array.
func TestVacancyPrefilterFieldsFromLiveSearchItem(t *testing.T) {
	v := vacancyFromItem(json.RawMessage(readLive(t, "search_item_badges.json")))
	if v.TotalResponsesCount != 700 {
		t.Fatalf("TotalResponsesCount = %d, want 700", v.TotalResponsesCount)
	}
	if len(v.UserLabels) != 0 {
		t.Fatalf("UserLabels = %v, want empty", v.UserLabels)
	}
}

// T115: a labeled item (donor semantics: non-empty userLabels marks the
// vacancy the applicant already interacted with) parses the label strings.
func TestVacancyPrefilterFieldsLabeledItem(t *testing.T) {
	v := vacancyFromItem(json.RawMessage(`{"vacancyId":1,"name":"x","userLabels":["already-responded"],"totalResponsesCount":42}`))
	if len(v.UserLabels) != 1 || v.UserLabels[0] != "already-responded" {
		t.Fatalf("UserLabels = %v, want [already-responded]", v.UserLabels)
	}
	if v.TotalResponsesCount != 42 {
		t.Fatalf("TotalResponsesCount = %d, want 42", v.TotalResponsesCount)
	}
}

// T115 regression: absent or empty fields change nothing (cards and legacy
// shapes carry neither field — the typed values stay zero).
func TestVacancyPrefilterFieldsAbsent(t *testing.T) {
	for _, raw := range []string{
		`{"vacancyId":1,"name":"x"}`,
		`{"vacancyId":1,"name":"x","userLabels":[],"totalResponsesCount":0}`,
	} {
		v := vacancyFromItem(json.RawMessage(raw))
		if v.UserLabels != nil {
			t.Fatalf("UserLabels = %v, want nil (%s)", v.UserLabels, raw)
		}
		if v.TotalResponsesCount != 0 {
			t.Fatalf("TotalResponsesCount = %d, want 0 (%s)", v.TotalResponsesCount, raw)
		}
	}
}
