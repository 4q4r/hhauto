package hhapi

import (
	"testing"
	"time"
)

// T36: the server lift timer surfaces as _attributes.nextTouchAt. Live hh
// serves a ms epoch; other surfaces string ISO times — both must parse,
// null/absent must stay zero (autolift falls back to updated_at + 4h).
func TestParseResumesNextTouchAt(t *testing.T) {
	body := `{"resumes":[
		{"hash":"ms","_attributes":{"status":"published","isSearchable":true,"nextTouchAt":1790510400000}},
		{"hash":"iso","_attributes":{"status":"published","isSearchable":true,"nextTouchAt":"2026-09-24T12:00:00+0300"}},
		{"hash":"null","_attributes":{"status":"published","isSearchable":true,"nextTouchAt":null}},
		{"hash":"absent","_attributes":{"status":"published","isSearchable":true}}
	]}`
	rs, err := parseResumes([]byte(body))
	if err != nil {
		t.Fatalf("parseResumes: %v", err)
	}
	if len(rs) != 4 {
		t.Fatalf("resumes = %d, want 4", len(rs))
	}
	if want := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC); !rs[0].NextTouchAt.Equal(want) {
		t.Fatalf("ms epoch nextTouchAt = %v, want %v", rs[0].NextTouchAt, want)
	}
	if want := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC); !rs[1].NextTouchAt.Equal(want) {
		t.Fatalf("ISO nextTouchAt = %v, want %v", rs[1].NextTouchAt, want)
	}
	if !rs[2].NextTouchAt.IsZero() || !rs[3].NextTouchAt.IsZero() {
		t.Fatalf("null/absent nextTouchAt = %v/%v, want zero", rs[2].NextTouchAt, rs[3].NextTouchAt)
	}
}
