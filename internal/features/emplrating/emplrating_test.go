package emplrating

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "empl.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeAPI is a minimal hhapi stub for filter tests (only EmployerRating runs).
type fakeAPI struct {
	hhapi.HHAPI
	rating *hhapi.EmployerRating
	err    error
	calls  int
}

func (f *fakeAPI) EmployerRating(_ context.Context, _ int64) (*hhapi.EmployerRating, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rating, nil
}

func rating(id int64, scoreAll, scoreRecent float64, reviews int) *hhapi.EmployerRating {
	return &hhapi.EmployerRating{
		EmployerID: id, ScoreAll: scoreAll, ScoreRecent: scoreRecent,
		ReviewsCount: reviews, FetchedAt: time.Now(),
	}
}

func TestThresholdNotConfiguredPassesWithoutAPICall(t *testing.T) {
	api := &fakeAPI{}
	db := testDB(t)
	f := New(db, api, nil)
	pass, reason := f.Pass(context.Background(), "acc", 9001, config.RunFilters{})
	if !pass || reason != "rating_not_configured" {
		t.Fatalf("pass=%v reason=%q, want true/rating_not_configured", pass, reason)
	}
	if api.calls != 0 {
		t.Fatalf("api calls = %d, want 0", api.calls)
	}
}

func TestFreshCacheHitSkipsAPI(t *testing.T) {
	api := &fakeAPI{rating: rating(9001, 4.5, 4.8, 100)}
	db := testDB(t)
	f := New(db, api, nil)
	flt := config.RunFilters{MinEmployerRating: 4.0}

	if pass, _ := f.Pass(context.Background(), "acc", 9001, flt); !pass {
		t.Fatalf("first pass = false, want true")
	}
	if pass, _ := f.Pass(context.Background(), "acc", 9001, flt); !pass {
		t.Fatalf("second pass = false, want true")
	}
	if api.calls != 1 {
		t.Fatalf("api calls = %d, want 1 (cache hit on second call)", api.calls)
	}
}

func TestStaleCacheRefetches(t *testing.T) {
	api := &fakeAPI{rating: rating(9002, 4.5, 4.8, 100)}
	db := testDB(t)
	now := time.Now()
	clock := &now // mutable
	f := New(db, api, nil)
	f.now = func() time.Time { return *clock }
	flt := config.RunFilters{MinEmployerRating: 4.0, RatingCacheHours: 2}

	if pass, _ := f.Pass(context.Background(), "acc", 9002, flt); !pass {
		t.Fatalf("first pass = false")
	}
	*clock = now.Add(3 * time.Hour) // cache expired
	if pass, _ := f.Pass(context.Background(), "acc", 9002, flt); !pass {
		t.Fatalf("second pass = false")
	}
	if api.calls != 2 {
		t.Fatalf("api calls = %d, want 2 (stale cache refetch)", api.calls)
	}
}

func TestPolicyAllUsesScoreAll(t *testing.T) {
	db := testDB(t)
	flt := config.RunFilters{MinEmployerRating: 4.0, RatingPeriod: "all"}

	api := &fakeAPI{rating: rating(9003, 4.2, 2.0, 50)}
	pass, reason := New(db, api, nil).Pass(context.Background(), "acc", 9003, flt)
	if !pass || reason != "rating_ok" {
		t.Fatalf("pass=%v reason=%q, want true/rating_ok", pass, reason)
	}

	api = &fakeAPI{rating: rating(9004, 3.9, 5.0, 50)}
	pass, reason = New(db, api, nil).Pass(context.Background(), "acc", 9004, flt)
	if pass || reason != "rating_low_all" {
		t.Fatalf("pass=%v reason=%q, want false/rating_low_all", pass, reason)
	}
}

func TestPolicyRecentUsesScoreRecent(t *testing.T) {
	db := testDB(t)
	flt := config.RunFilters{MinEmployerRating: 4.0, RatingPeriod: "90"}

	api := &fakeAPI{rating: rating(9005, 5.0, 4.1, 50)}
	if pass, _ := New(db, api, nil).Pass(context.Background(), "acc", 9005, flt); !pass {
		t.Fatalf("want pass on recent score")
	}
	api = &fakeAPI{rating: rating(9006, 5.0, 3.5, 50)}
	pass, reason := New(db, api, nil).Pass(context.Background(), "acc", 9006, flt)
	if pass || reason != "rating_low_recent" {
		t.Fatalf("pass=%v reason=%q, want false/rating_low_recent", pass, reason)
	}
}

func TestNoReviewsPolicies(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// allow (default when unset)
	api := &fakeAPI{rating: rating(9007, 0, 0, 0)}
	pass, reason := New(db, api, nil).Pass(ctx, "acc", 9007, config.RunFilters{MinEmployerRating: 4.0})
	if !pass || reason != "no_reviews_allowed" {
		t.Fatalf("default: pass=%v reason=%q, want true/no_reviews_allowed", pass, reason)
	}

	// skip
	api = &fakeAPI{rating: rating(9008, 0, 0, 0)}
	pass, reason = New(db, api, nil).Pass(ctx, "acc", 9008,
		config.RunFilters{MinEmployerRating: 4.0, NoReviewsPolicy: "skip"})
	if pass || reason != "no_reviews" {
		t.Fatalf("skip: pass=%v reason=%q, want false/no_reviews", pass, reason)
	}

	// human: skip + review item persisted
	api = &fakeAPI{rating: rating(9009, 0, 0, 0)}
	pass, reason = New(db, api, nil).Pass(ctx, "acc", 9009,
		config.RunFilters{MinEmployerRating: 4.0, NoReviewsPolicy: "human"})
	if pass || reason != "no_reviews_human" {
		t.Fatalf("human: pass=%v reason=%q, want false/no_reviews_human", pass, reason)
	}
	open, err := db.ReviewItemsOpen(ctx, "acc", 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("open review items = %d err=%v, want 1", len(open), err)
	}
	if open[0].Kind != "emplrating_no_reviews" {
		t.Fatalf("review kind = %q", open[0].Kind)
	}
}

func TestFetchErrorFailsOpenWithLog(t *testing.T) {
	db := testDB(t)
	api := &fakeAPI{err: errors.New("widget down")}
	pass, reason := New(db, api, nil).Pass(context.Background(), "acc", 9010,
		config.RunFilters{MinEmployerRating: 4.0})
	if !pass || reason != "rating_fetch_error" {
		t.Fatalf("pass=%v reason=%q, want true/rating_fetch_error (fail-open)", pass, reason)
	}
}

func TestRatingPersistedToStore(t *testing.T) {
	db := testDB(t)
	api := &fakeAPI{rating: rating(9011, 4.4, 4.6, 77)}
	_, _ = New(db, api, nil).Pass(context.Background(), "acc", 9011,
		config.RunFilters{MinEmployerRating: 4.0})
	got, err := db.EmployerRatingsGet(context.Background(), 9011)
	if err != nil || got == nil {
		t.Fatalf("store rating missing: %v", err)
	}
	if got.ScoreAll != 4.4 || got.ReviewsCount != 77 {
		t.Fatalf("stored = %+v", got)
	}
}
