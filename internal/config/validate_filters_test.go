package config

import (
	"strings"
	"testing"
)

// --- T25: enum validation for the extended run filters (hh silently ignores
// unknown values, so fail-fast startup validation is the only defense) ---

func TestValidateAcceptsExtendedFilterEnums(t *testing.T) {
	c := validConfig()
	mutateRun(c, "daily", func(r *Run) {
		r.Filters.ProfessionalRoles = []string{"160", "96"}
		r.Filters.Metros = []string{"532"}
		r.Filters.WorkFormats = []string{"REMOTE", "HYBRID", "ON_SITE"}
		r.Filters.Labels = []string{"with_salary", "totally_new_future_label"} // free-form by design
		r.Filters.Part = []string{"temp", "evening", "weekend"}
		r.Filters.OnlyWithSalary = true
		r.Filters.OrderBy = "publication_time_desc"
	})
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(valid extended filters) = %v, want nil", err)
	}
}

func TestValidateAcceptsEveryOrderByValue(t *testing.T) {
	for _, ob := range []string{"", "relevance", "publication_time_desc", "salary_desc", "salary_asc"} {
		c := validConfig()
		mutateRun(c, "daily", func(r *Run) { r.Filters.OrderBy = ob })
		if err := Validate(c); err != nil {
			t.Fatalf("Validate(order_by=%q) = %v, want nil", ob, err)
		}
	}
}

func TestValidateRejectsBadWorkFormat(t *testing.T) {
	// "ONSITE" (no underscore) is silently ignored by hh (live-verified
	// 2026-09-25): must be rejected at startup, not swallowed.
	for _, bad := range []string{"ONSITE", "onsite", "REMOTE "} {
		c := validConfig()
		mutateRun(c, "daily", func(r *Run) { r.Filters.WorkFormats = []string{"REMOTE", bad} })
		err := Validate(c)
		if err == nil {
			t.Fatalf("Validate(work_formats containing %q) = nil, want error", bad)
		}
		if !strings.Contains(err.Error(), "work_formats") {
			t.Fatalf("error %v must name filters.work_formats", err)
		}
	}
}

func TestValidateRejectsBadPartValue(t *testing.T) {
	c := validConfig()
	mutateRun(c, "daily", func(r *Run) { r.Filters.Part = []string{"evening", "nights"} })
	err := Validate(c)
	if err == nil {
		t.Fatal("Validate(part=nights) = nil, want error")
	}
	if !strings.Contains(err.Error(), "part") {
		t.Fatalf("error %v must name filters.part", err)
	}
}

func TestValidateRejectsBadOrderBy(t *testing.T) {
	c := validConfig()
	mutateRun(c, "daily", func(r *Run) { r.Filters.OrderBy = "salary" })
	err := Validate(c)
	if err == nil {
		t.Fatal("Validate(order_by=salary) = nil, want error")
	}
	if !strings.Contains(err.Error(), "order_by") {
		t.Fatalf("error %v must name filters.order_by", err)
	}
}

func TestValidateSkipsEmptyEnumEntries(t *testing.T) {
	c := validConfig()
	mutateRun(c, "daily", func(r *Run) {
		r.Filters.WorkFormats = []string{""}
		r.Filters.Part = []string{""}
	})
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(empty enum entries) = %v, want nil (empties are skipped on the wire)", err)
	}
}
