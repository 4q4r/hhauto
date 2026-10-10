package jobsearch

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// T118: the daily «Ищу работу» tick — flag off = dead code, flag on = one
// status POST per account per 24h, gated by the feature_state marker.

var testNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type fakeAPI struct {
	hhapi.HHAPI // nil-embedded: unexpected calls panic loudly

	mu    sync.Mutex
	calls int
	err   error // sticky error served on every call
}

func (f *fakeAPI) SetJobSearchStatus(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	return nil
}

// Only SetJobSearchStatus is ever called; the nil-embedded HHAPI panics on
// anything else.

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir()+"/test.db", store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	db.Now = func() time.Time { return testNow }
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type harness struct {
	api  *fakeAPI
	db   *store.DB
	opts Options
}

func newHarness(t *testing.T, accounts ...string) *harness {
	t.Helper()
	accs := map[string]config.Account{}
	for _, id := range accounts {
		accs[id] = config.Account{DisplayName: id}
	}
	api := &fakeAPI{}
	h := &harness{api: api, db: openDB(t)}
	h.opts = Options{
		Enabled: true,
		Now:     func() time.Time { return testNow },
		Log:     slog.New(slog.DiscardHandler),
		DB:      h.db,
		APIFor: func(_ string) (hhapi.HHAPI, error) {
			return api, nil
		},
		Accounts: accounts,
	}
	return h
}

func TestTickDisabled(t *testing.T) {
	h := newHarness(t, "acc1")
	h.opts.Enabled = false
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if h.api.calls != 0 {
		t.Fatalf("calls = %d, want 0 (flag off = dead code)", h.api.calls)
	}
}

func TestTickSetsStatusOncePerDay(t *testing.T) {
	h := newHarness(t, "acc1")
	// First maintenance tick: no marker → POST + stamp.
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	if h.api.calls != 1 {
		t.Fatalf("calls = %d, want 1", h.api.calls)
	}
	// A later maintenance run the same day: the 24h gate holds → no POST.
	h.opts.Now = func() time.Time { return testNow.Add(23 * time.Hour) }
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if h.api.calls != 1 {
		t.Fatalf("calls = %d, want 1 (daily gate)", h.api.calls)
	}
	// Past 24h: the daily point fires again.
	h.opts.Now = func() time.Time { return testNow.Add(24 * time.Hour) }
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("third Tick: %v", err)
	}
	if h.api.calls != 2 {
		t.Fatalf("calls = %d, want 2 (next daily point)", h.api.calls)
	}
}

func TestTickEveryAccountIndependently(t *testing.T) {
	h := newHarness(t, "acc1", "acc2")
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if h.api.calls != 2 {
		t.Fatalf("calls = %d, want 2 (one per account)", h.api.calls)
	}
	// Both accounts are stamped: a same-day repeat is a no-op.
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("repeat Tick: %v", err)
	}
	if h.api.calls != 2 {
		t.Fatalf("calls = %d, want 2", h.api.calls)
	}
}

func TestTickFailureDoesNotStamp(t *testing.T) {
	h := newHarness(t, "acc1")
	h.api.mu.Lock()
	h.api.err = errors.New("boom")
	h.api.mu.Unlock()
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("Tick must keep the failure per-account (got %v)", err)
	}
	if h.api.calls != 1 {
		t.Fatalf("calls = %d, want 1", h.api.calls)
	}
	// No stamp → the next maintenance run retries.
	h.api.mu.Lock()
	h.api.err = nil
	h.api.mu.Unlock()
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("retry Tick: %v", err)
	}
	if h.api.calls != 2 {
		t.Fatalf("calls = %d, want 2 (failed tick left no marker)", h.api.calls)
	}
}

func TestTickCaptchaRetriesNextRun(t *testing.T) {
	h := newHarness(t, "acc1")
	h.api.mu.Lock()
	h.api.err = &hhapi.StatusError{Kind: hhapi.ErrCaptcha, StatusCode: http.StatusOK}
	h.api.mu.Unlock()
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Captcha left no stamp: the next run retries (and stops failing).
	h.api.mu.Lock()
	h.api.err = nil
	h.api.mu.Unlock()
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("retry Tick: %v", err)
	}
	if h.api.calls != 2 {
		t.Fatalf("calls = %d, want 2", h.api.calls)
	}
}

func TestTickMarkerIsPersisted(t *testing.T) {
	h := newHarness(t, "acc1")
	if err := Tick(context.Background(), h.opts); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	v, ok, err := h.db.FeatureStateGet(context.Background(), "jobsearch", "acc1", markerKey)
	if err != nil || !ok {
		t.Fatalf("marker: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(v, "2026-10-10T12:00") {
		t.Fatalf("marker = %q, want the RFC3339 stamp of the flow clock", v)
	}
}
