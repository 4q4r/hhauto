package hhapi

// Badge label extraction (moved from internal/console, T69) over the live
// T70 fact: search items carry company.badges.badge[].

import (
	"encoding/json"
	"strings"
	"testing"
)

// Badges: the flat array, the SSR nesting, the employer nest (cards), the
// company nest (search items, live T70), unknown types as-is, no payload →
// empty.
func TestBadgesShapes(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"badges":[{"type":"employer-it-accreditation"}]}`, "аккредитованный ИТ"},
		{`{"badges":{"badge":[{"type":"employer-it-accreditation"}]}}`, "аккредитованный ИТ"},
		{`{"employer":{"badges":{"badge":[{"type":"employer-it-accreditation"}]}}}`, "аккредитованный ИТ"},
		{`{"company":{"badges":{"badge":[{"type":"employer-hh-rating"}]}}}`, "рейтинг hh"},
		{`{"badges":[{"type":"future-unknown-badge"}]}`, "future-unknown-badge"},
		{`{}`, ""},
		{`{"badges":[]}`, ""},
	}
	for _, tc := range cases {
		got := strings.Join(Badges(json.RawMessage(tc.raw)), ",")
		if got != tc.want {
			t.Fatalf("Badges(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if Badges(nil) != nil {
		t.Fatal("nil raw must yield no badges")
	}
}

// T70 golden: vacancyFromItem over the depersonalized live search item
// (company.badges.badge[] with employer-hh-rating + employer-it-accreditation)
// fills the typed Vacancy.Badges in capture order.
func TestVacancyBadgesFromLiveSearchItem(t *testing.T) {
	v := vacancyFromItem(json.RawMessage(readLive(t, "search_item_badges.json")))
	want := []string{"рейтинг hh", "аккредитованный ИТ"}
	if len(v.Badges) != len(want) {
		t.Fatalf("Badges = %v, want %v", v.Badges, want)
	}
	for i := range want {
		if v.Badges[i] != want[i] {
			t.Fatalf("Badges = %v, want %v", v.Badges, want)
		}
	}
}
