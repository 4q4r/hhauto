package hhapi

import (
	"net/http"
	"strings"
	"testing"
)

// --- T25: full coverage of hh search filters (everything expressible) ---

// TestSearchQueryFullFilterMapping pins the searchURL mapping for every
// extended filter: multi-value params keep order and repeat the key, scalar
// params are set once. Live-verified 2026-09-25 via /shards/vacancy/counts:
// professional_role, metro, work_format (REMOTE|HYBRID|ON_SITE), label
// (multi, OR-semantics), only_with_salary and order_by all narrow/accepted.
func TestSearchQueryFullFilterMapping(t *testing.T) {
	env := newTestEnv(t)
	var gotQuery string
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.SearchVacancies(ctx, SearchQuery{
		Text:              "devops",
		ProfessionalRoles: []string{"160", "96"},
		Metros:            []string{"532", "531"},
		WorkFormats:       []string{"REMOTE", "HYBRID", "ON_SITE"},
		Labels:            []string{"with_salary", "with_address"},
		Part:              []string{"temp", "evening", "weekend"},
		OnlyWithSalary:    true,
		OrderBy:           "publication_time_desc",
		PerPage:           50,
	})
	if err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	for _, want := range []string{
		"professional_role=160", "professional_role=96", // multi, order kept
		"metro=532", "metro=531",
		"work_format=REMOTE", "work_format=HYBRID", "work_format=ON_SITE",
		"label=with_salary", "label=with_address",
		"part=temp", "part=evening", "part=weekend",
		"only_with_salary=true",
		"order_by=publication_time_desc",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
}

// TestSearchQueryLabelMergesNoAgencies: LabelNoAgencies and the free-form
// Labels list must both reach the wire as repeated label= params (hh applies
// OR-semantics inside a param family).
func TestSearchQueryLabelMergesNoAgencies(t *testing.T) {
	env := newTestEnv(t)
	var gotQuery string
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if _, err := c.SearchVacancies(ctx, SearchQuery{
		Text:            "devops",
		LabelNoAgencies: true,
		Labels:          []string{"with_salary"},
		PerPage:         50,
	}); err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	if !strings.Contains(gotQuery, "label=not_from_agency") {
		t.Fatalf("query %q missing label=not_from_agency", gotQuery)
	}
	if !strings.Contains(gotQuery, "label=with_salary") {
		t.Fatalf("query %q missing label=with_salary", gotQuery)
	}
}

// TestSearchQueryEmptyExtendedFiltersOmitted: zero values of the extended
// filters must not produce empty or dangling params.
func TestSearchQueryEmptyExtendedFiltersOmitted(t *testing.T) {
	env := newTestEnv(t)
	var gotQuery string
	env.serve(env.mux1, "1", "GET", "/search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(readFixture(t, "search.html"))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if _, err := c.SearchVacancies(ctx, SearchQuery{
		Text:              "devops",
		ProfessionalRoles: []string{"", "160"}, // empty entries skipped, valid kept
		Metros:            nil,
		WorkFormats:       []string{},
		Labels:            []string{""},
		Part:              nil,
		PerPage:           50,
	}); err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	for _, banned := range []string{"metro=", "work_format=", "part=", "only_with_salary", "order_by", "label="} {
		if strings.Contains(gotQuery, banned) {
			t.Fatalf("query %q must not contain %q", gotQuery, banned)
		}
	}
	if !strings.Contains(gotQuery, "professional_role=160") {
		t.Fatalf("query %q missing professional_role=160", gotQuery)
	}
}
