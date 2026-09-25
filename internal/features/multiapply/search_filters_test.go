package multiapply

import (
	"strings"
	"testing"

	"hhauto/internal/config"
)

// T25 regression: every extended run filter must survive the
// run.Filters -> hhapi.SearchQuery wiring and reach the search page.
func TestExtendedFiltersReachSearchQuery(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.MinEmployerRating = 0
		r.Filters.ProfessionalRoles = []string{"160"}
		r.Filters.Metros = []string{"532"}
		r.Filters.WorkFormats = []string{"REMOTE", "HYBRID"}
		r.Filters.Labels = []string{"with_salary"}
		r.Filters.Part = []string{"evening"}
		r.Filters.OnlyWithSalary = true
		r.Filters.OrderBy = "publication_time_desc"
		c.Runs["run1"] = r
	})
	h.searchItems = []string{vacItem(777001, "Go Backend", 9001, "Corp LLC", "")}
	h.searchFound = 1
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777001] = popupWith(true, false, "ENABLED")

	h.run(t)

	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777001 {
		t.Fatalf("apply calls = %v, want [777001]", h.applyCalls)
	}
	if len(h.searchQueryLog) == 0 {
		t.Fatal("no search request captured")
	}
	q := h.searchQueryLog[0]
	for _, want := range []string{
		"professional_role=160",
		"metro=532",
		"work_format=REMOTE", "work_format=HYBRID",
		"label=with_salary",
		"part=evening",
		"only_with_salary=true",
		"order_by=publication_time_desc",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("search query %q missing %q", q, want)
		}
	}
}
