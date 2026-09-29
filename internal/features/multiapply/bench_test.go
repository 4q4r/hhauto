package multiapply

// BenchmarkCascade measures the DB-backed filter chain of processVacancy
// (F2 exclude → F3 blacklist → F4 employer-rating cache hit → F5 dedup →
// F7 quota) with NO network: the rating is served from a seeded
// employer_ratings row (the cache the production filter reads), and the
// exhausted MaxPerDay quota stops the cascade before the F6.5 popup fetch.
// It is the per-vacancy fixed cost paid before any I/O to hh.ru.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

func BenchmarkCascade(b *testing.B) {
	ctx := context.Background()
	db, err := store.Open(context.Background(), filepath.Join(b.TempDir(), "cascade.db"), store.Options{})
	if err != nil {
		b.Fatalf("store.Open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	// F4 pre-heat: a fresh cache row (write-through shape) so every Pass
	// is a cache hit — emplrating never touches its (nil) API.
	fetched := time.Now().UTC()
	if err := db.EmployerRatingsPut(ctx, store.EmployerRating{
		EmployerID: 555, ScoreAll: 4.5, ScoreRecent: 4.6,
		ReviewsCount: 100, RecommendPct: 90, FetchedAt: fetched,
	}); err != nil {
		b.Fatal(err)
	}

	// F7 pre-heat: one applied row today on run1 → the very first
	// processVacancy stops at run_quota (after F2..F5, before the popup).
	seededAt := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	if err := db.ApplicationsCreate(ctx, &store.Application{
		AccountID: "acc1", RunID: "run1", HhResumeID: "hash1",
		VacancyID: 1, EmployerID: 999999, DedupKey: "acc1|hash1|1",
		Status: "applied", AppliedAt: &seededAt,
	}); err != nil {
		b.Fatal(err)
	}

	cfg := config.Config{
		General:  config.General{Timezone: "UTC", DryRun: false},
		Accounts: map[string]config.Account{"acc1": {DisplayName: "acc1", UserAgent: "bench", RPS: 50, DailyApplyCap: 200}},
		Features: config.Features{MultiApply: config.MultiApply{Enabled: true, AIFilter: false}},
	}
	run := config.Run{
		Enabled: true, Account: "acc1", ResumeID: "hash1", Query: "go",
		MaxPerDay: 1,
		Filters: config.RunFilters{
			ExcludeWords:      []string{"стажировка", "intern", "zzznomatch"},
			ExcludeInDesc:     true,
			CompanyBlacklist:  []string{"999999", "BadCorp"},
			MinEmployerRating: 4.0,
		},
	}
	f := New(Options{
		Config: cfg,
		DB:     db,
		APIFor: func(string) (hhapi.HHAPI, error) { return nil, nil }, // never reached: quota stops first
		Now:    func() time.Time { return now },                       // harness clock (fake_test.go)
		Log:    quietLogger(),
	})
	prof := profile.Profile{
		AccountID:    "acc1",
		ResumeDigest: "Резюме: Go, PostgreSQL, 5 лет",
		ProfileText:  "Ищу бэкенд-роли",
		Facts:        map[string]string{"target_roles": "Go-разработчик"},
	}
	base := hhapi.Vacancy{
		ID: 424242, Name: "Go-разработчик (backend, платформа)",
		EmployerID: 555, EmployerName: "GoodCorp",
		Area: "Таганрог", Description: "Микросервисы, PostgreSQL, Kafka. Оформление по ТК РФ.",
	}

	b.ReportAllocs()
	n := 0
	for b.Loop() {
		n++
		v := base
		v.ID = int64(424241 + n) // fresh dedup key per op: the F5 read is real
		stop, skip := f.processVacancy(ctx, "run1", run, "acc1", nil, prof, "hash1", v, map[int64]bool{}, nil, newPassStats())
		if stop != "run_quota" || skip != "" {
			b.Fatalf("stop=%q skip=%q, want run_quota (cascade must stop after the filters, before the popup)", stop, skip)
		}
	}
}
