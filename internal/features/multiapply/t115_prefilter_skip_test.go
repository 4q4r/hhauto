package multiapply

// T115: the search-item pre-filters — a labeled vacancy (the applicant's
// own hh labels, donor semantics: an "already responded" marker) and a
// vacancy whose response counter exceeds the cap skip before the
// popup/card. Both knobs default off: an untouched config changes nothing.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

func newPrefilterFeature(t *testing.T, ma config.MultiApply, log *slog.Logger) (*Feature, config.Run) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t115.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if log == nil {
		log = quietLogger()
	}
	cfg := config.Config{
		General:  config.General{Timezone: "UTC"},
		Accounts: map[string]config.Account{"acc1": {DisplayName: "acc1", UserAgent: "u", RPS: 50, DailyApplyCap: 200}},
		Features: config.Features{MultiApply: ma},
	}
	f := New(Options{
		Config: cfg,
		DB:     db,
		APIFor: func(string) (hhapi.HHAPI, error) { return nil, nil }, // never reached: the pre-filters stop first
		Now:    func() time.Time { return now },
		Log:    log,
	})
	return f, config.Run{Enabled: true, Account: "acc1", ResumeID: "hash1", Query: "go"}
}

func evaluate(t *testing.T, f *Feature, run config.Run, v hhapi.Vacancy) (string, *candidate) {
	t.Helper()
	skip, cand := f.evaluateVacancy(context.Background(), "run1", run, "acc1", nil,
		profile.Profile{AccountID: "acc1"}, "hash1", v, map[int64]bool{}, nil)
	return skip, cand
}

func TestSkipWhenLabeled(t *testing.T) {
	f, run := newPrefilterFeature(t, config.MultiApply{Enabled: true, SkipWhenLabeled: true}, nil)
	skip, cand := evaluate(t, f, run, hhapi.Vacancy{ID: 101, UserLabels: []string{"already-responded"}})
	if skip != "user_labeled" || cand != nil {
		t.Fatalf("skip=%q, want user_labeled", skip)
	}
}

func TestLabeledVacancyKeptWhenOff(t *testing.T) {
	f, run := newPrefilterFeature(t, config.MultiApply{Enabled: true}, nil)
	skip, cand := evaluate(t, f, run, hhapi.Vacancy{ID: 102, UserLabels: []string{"already-responded"}})
	if skip != "" || cand == nil {
		t.Fatalf("skip=%q, want empty (default must not skip)", skip)
	}
}

func TestSkipMaxTotalResponses(t *testing.T) {
	f, run := newPrefilterFeature(t, config.MultiApply{Enabled: true, MaxTotalResponses: 100}, nil)
	for _, tc := range []struct {
		count int64
		want  string
	}{
		{700, "responses_over"},
		{101, "responses_over"},
		{100, ""}, // strictly greater
		{0, ""},
	} {
		skip, _ := evaluate(t, f, run, hhapi.Vacancy{ID: 200 + tc.count, TotalResponsesCount: int(tc.count)})
		if skip != tc.want {
			t.Fatalf("count=%d: skip=%q, want %q", tc.count, skip, tc.want)
		}
	}
}

func TestResponseCapOffByDefault(t *testing.T) {
	f, run := newPrefilterFeature(t, config.MultiApply{Enabled: true}, nil)
	skip, cand := evaluate(t, f, run, hhapi.Vacancy{ID: 301, TotalResponsesCount: 700})
	if skip != "" || cand == nil {
		t.Fatalf("skip=%q, want empty (cap 0 = off)", skip)
	}
}

func TestPrefilterValuesLoggedOnSkip(t *testing.T) {
	var buf bytes.Buffer
	f, run := newPrefilterFeature(t, config.MultiApply{Enabled: true, SkipWhenLabeled: true},
		slog.New(slog.NewTextHandler(&buf, nil)))
	skip, _ := evaluate(t, f, run, hhapi.Vacancy{ID: 401, UserLabels: []string{"already-responded"}, TotalResponsesCount: 55})
	if skip != "user_labeled" {
		t.Fatalf("skip=%q, want user_labeled", skip)
	}
	for _, frag := range []string{"401", "already-responded", "55"} {
		if !strings.Contains(buf.String(), frag) {
			t.Errorf("values log missing %q: %s", frag, buf.String())
		}
	}
}
