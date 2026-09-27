package autolift

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
)

// --- stateful fake hh server ---

type fakeResume struct {
	Hash       string
	Title      string
	Status     string
	Searchable bool
	Updated    time.Time
	NextTouch  time.Time // T36: served as _attributes.nextTouchAt (ms epoch)
}

// hhFake is a stateful hh.ru fake wired through a real hhapi client (the
// internal/hhapi httptest pattern): ListResumes and the XSRF bootstrap read
// /shards/applicant/resumes, TouchResume posts /applicant/resumes/touch, and
// the settings SSR page stays as the XSRF fallback source.
type hhFake struct {
	t   *testing.T
	srv *httptest.Server
	api hhapi.HHAPI

	mu        sync.Mutex
	resumes   []fakeResume
	touch     int // successful touches
	lists     int // shard GETs (T36: server-timer passes must not re-list)
	touchErr  func(call int) error
	noopTouch bool      // success without bumping updated_at (verify-failure mode)
	updated   time.Time // updated_at served after a successful touch
}

func newHHFake(t *testing.T, _ *store.DB, resumes ...fakeResume) *hhFake {
	t.Helper()
	f := &hhFake{t: t, resumes: resumes, updated: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_test", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, xsrfPage)
	})
	mux.HandleFunc("GET /shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lists++
		_, _ = io.WriteString(w, f.resumesJSON())
	})
	mux.HandleFunc("POST /applicant/resumes/touch", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		calls := f.touch + 1
		f.touch = calls
		fn, noop := f.touchErr, f.noopTouch
		f.mu.Unlock()
		if fn != nil {
			if err := fn(calls); err != nil {
				if se, ok := err.(*touchError); ok && se.redirect != "" {
					http.Redirect(w, r, se.redirect, http.StatusFound)
					return
				}
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		if !noop {
			f.mu.Lock()
			for i := range f.resumes {
				f.resumes[i].Updated = f.updated
			}
			f.mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	c, err := hhapi.NewClient(
		config.Account{DisplayName: "acc1", UserAgent: "test-ua/1.0", RPS: 50},
		config.APIEndpoints{Site: f.srv.URL, Chatik: f.srv.URL, SearchPage: f.srv.URL + "/search/vacancy"},
		nil, nil,
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	f.api = c
	return f
}

type touchError struct {
	redirect string
	msg      string
}

func (e *touchError) Error() string { return e.msg }

func (f *hhFake) resumesJSON() string {
	var b strings.Builder
	b.WriteString(`{"resumes":[`)
	for i, r := range f.resumes {
		if i > 0 {
			b.WriteString(",")
		}
		nt := "null"
		if !r.NextTouch.IsZero() {
			nt = strconv.FormatInt(r.NextTouch.UnixMilli(), 10)
		}
		fmt.Fprintf(&b, `{"hash":%q,"title":%q,"updated_at":%q,"_attributes":{"percent":90,`+
			`"status":{"id":%q,"name":%q},"isSearchable":%t,"canPublishOrUpdate":true,"nextTouchAt":%s}}`,
			r.Hash, r.Title, r.Updated.In(fixedLoc).Format("2006-01-02T15:04:05-0700"),
			r.Status, statusName(r.Status), r.Searchable, nt)
	}
	b.WriteString(`]}`)
	return b.String()
}

// statusName renders the display name the SSR state wraps: the hh parser
// surfaces the wrapped "name" first, so the fake mirrors the real shape
// ("published" → "Опубликовано").
func statusName(status string) string {
	if status == "published" {
		return "Опубликовано"
	}
	return status
}

// fixedLoc renders fake times with a stable +0300 offset like real hh.
var fixedLoc = time.FixedZone("MSK", 3*3600)

const xsrfPage = `<!DOCTYPE html><html><body><template id="HH-Lux-InitialState">` +
	`{&#34;xsrfToken&#34;:&#34;tok_test&#34;,&#34;session&#34;:{&#34;userId&#34;:1}}</template></body></html>`

// --- harness ---

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "lift.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var now = time.Date(2026, 9, 24, 10, 15, 0, 0, time.UTC)

type harness struct {
	db    *store.DB
	fake  *hhFake
	chain *fakeChain
	cfg   config.Config
}

func newHarness(t *testing.T, mutate func(*config.Config), resumes ...fakeResume) *harness {
	t.Helper()
	db := testDB(t)
	fake := newHHFake(t, db, resumes...)
	h := &harness{
		db:    db,
		fake:  fake,
		chain: &fakeChain{},
		cfg: config.Config{
			General:  config.General{Timezone: "UTC"},
			Accounts: map[string]config.Account{"acc1": {DisplayName: "acc1", UserAgent: "u", RPS: 50, DailyApplyCap: 200}},
			Features: config.Features{AutoLift: config.AutoLift{Enabled: true, PerDay: 6}},
		},
	}
	if mutate != nil {
		mutate(&h.cfg)
	}
	return h
}

func (h *harness) feature(t *testing.T) *Feature {
	t.Helper()
	f := New(Options{
		Config:   h.cfg,
		DB:       h.db,
		APIFor:   func(string) (hhapi.HHAPI, error) { return h.fake.api, nil },
		ChainFor: func(string) (captcha.Chain, bool) { return h.chain, true },
		Now:      func() time.Time { return now },
		Log:      quietLogger(),
	})
	return f
}

type fakeChain struct {
	solves int
	err    error
}

func (c *fakeChain) Solve(_ context.Context, _ captcha.Challenge) (captcha.Solution, error) {
	c.solves++
	if c.err != nil {
		return captcha.Solution{}, c.err
	}
	return captcha.Solution{Token: "tok", ProviderID: "fake"}, nil
}

// --- tests ---

func TestHappyPathTouchesVerifiesAndMarksDone(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Title: "Go dev", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}

	if got := h.fake.touch; got != 1 {
		t.Fatalf("touch calls = %d, want 1", got)
	}
	// verify read happened: touch bumped updated_at to h.fake.updated, and
	// the write-through persisted it.
	row, err := h.db.ResumesListByAccount(context.Background(), "acc1")
	if err != nil || len(row) != 1 {
		t.Fatalf("store resumes = %d err=%v", len(row), err)
	}
	if row[0].UpdatedAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("store updated_at = %v, want fresh (post-touch)", row[0].UpdatedAt)
	}
	// intent done for the window
	claim, err := featurekit.BeginIntent(context.Background(), h.db,
		"lift:hash1:"+featurekit.LiftWindow(now), "{}")
	if err != nil || claim.Step != featurekit.IntentReconcileFirst {
		t.Fatalf("intent claim = %+v err=%v, want reconcile-first (done)", claim, err)
	}
	// metric
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.ok"] != 1 {
		t.Fatalf("lift.ok metric = %d, want 1 (%v)", snap["lift.ok"], snap)
	}
}

func TestFourHourTimerSkipsFreshResume(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-1 * time.Hour)})
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 0 {
		t.Fatalf("touch calls = %d, want 0 (4h timer)", h.fake.touch)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.skip_timer"] != 1 {
		t.Fatalf("lift.skip_timer = %d (%v)", snap["lift.skip_timer"], snap)
	}
}

func TestUnpublishedOrHiddenResumeSkipped(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "h1", Status: "draft", Searchable: true, Updated: now.Add(-5 * time.Hour)},
		fakeResume{Hash: "h2", Status: "published", Searchable: false, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 0 {
		t.Fatalf("touch calls = %d, want 0", h.fake.touch)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.skip_unpublished"] != 2 {
		t.Fatalf("lift.skip_unpublished = %d (%v)", snap["lift.skip_unpublished"], snap)
	}
}

func TestDoneIntentSkipsSecondRunInSameWindow(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Force the local plan due: the done intent of this window must be
	// the hard barrier against a second touch.
	f.setPlan("acc1", map[string]time.Time{"hash1": now.Add(-time.Minute)})

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if h.fake.touch != 1 {
		t.Fatalf("touch calls = %d, want 1 (done intent is a hard barrier)", h.fake.touch)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.skip_intent"] != 1 {
		t.Fatalf("lift.skip_intent = %d (%v)", snap["lift.skip_intent"], snap)
	}
}

func TestCaptchaSolvedOnceThenSingleRetry(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	h.fake.touchErr = func(call int) error {
		if call == 1 {
			return &touchError{redirect: "/captcha/image?backurl=%2Fapplicant%2Fresumes"}
		}
		return nil
	}
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 2 {
		t.Fatalf("touch calls = %d, want 2 (captcha then single retry)", h.fake.touch)
	}
	if h.chain.solves != 1 {
		t.Fatalf("chain solves = %d, want 1", h.chain.solves)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.ok"] != 1 || snap["lift.captcha_retry"] != 1 {
		t.Fatalf("metrics = %v", snap)
	}
}

func TestCaptchaRetryFailureMarksFailed(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	h.fake.touchErr = func(int) error {
		return &touchError{redirect: "/captcha/image?backurl=%2Fapplicant%2Fresumes"}
	}
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 2 {
		t.Fatalf("touch calls = %d, want 2 (never a third)", h.fake.touch)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.ok"] != 0 || snap["lift.error"] != 1 {
		t.Fatalf("metrics = %v", snap)
	}
}

func TestVerifyFailureAbandonsIntentAndFlagsReview(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	// Touch reports success but the served updated_at never changes:
	// the verify-by-read must catch it.
	h.fake.noopTouch = true

	f := h.feature(t)
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}

	open, err := h.db.ReviewItemsOpen(context.Background(), "acc1", 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("open review items = %d err=%v, want 1", len(open), err)
	}
	if open[0].Kind != "lift_verify_failed" {
		t.Fatalf("review kind = %q", open[0].Kind)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.verify_failed"] != 1 {
		t.Fatalf("metrics = %v", snap)
	}
	// abandoned → the key can be re-armed on the next run.
	claim, _ := featurekit.BeginIntent(context.Background(), h.db,
		"lift:hash1:"+featurekit.LiftWindow(now), "{}")
	if claim.Step != featurekit.IntentProceed {
		t.Fatalf("claim step = %v, want proceed (abandoned replaced)", claim.Step)
	}
}

func TestDryRunMutatesNothing(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.General.DryRun = true },
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 0 {
		t.Fatalf("touch calls = %d, want 0 in dry-run", h.fake.touch)
	}
	// No intent row was created: a fresh claim still proceeds.
	claim, _ := featurekit.BeginIntent(context.Background(), h.db,
		"lift:hash1:"+featurekit.LiftWindow(now), "{}")
	if claim.Step != featurekit.IntentProceed {
		t.Fatalf("claim step = %v, want proceed (dry-run must not claim)", claim.Step)
	}
	snap, _ := h.db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if snap["lift.dry_run"] != 1 {
		t.Fatalf("metrics = %v", snap)
	}
}

func TestDisabledFeatureNotRunnableAndName(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.AutoLift.Enabled = false },
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)
	if f.Enabled() {
		t.Fatalf("Enabled() = true, want false")
	}
	if f.Name() != "autolift" {
		t.Fatalf("Name() = %q", f.Name())
	}
	if err := f.RunAccount(context.Background(), "acc1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("RunAccount err = %v, want ErrDisabled", err)
	}
}

func TestRegisterTasksSeedsWindowAndSpec(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	f := h.feature(t)
	q := queue.New()
	reg := scheduler.NewRegistry()

	if err := f.RegisterTasks(context.Background(), q, reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if _, ok := reg.Get("features.autolift"); !ok {
		t.Fatalf("registry missing features.autolift spec")
	}
	spec, _ := reg.Get("features.autolift")
	if len(spec.Times) != 6 {
		t.Fatalf("spec times = %v, want 6 runs/day", spec.Times)
	}
	if q.Len("acc1") != 1 {
		t.Fatalf("queued tasks = %d, want 1 per window", q.Len("acc1"))
	}
	// A second RegisterTasks must not double-seed (queue dedup).
	if err := f.RegisterTasks(context.Background(), q, reg); err != nil {
		t.Fatalf("second RegisterTasks: %v", err)
	}
	if q.Len("acc1") != 1 {
		t.Fatalf("queued tasks after re-register = %d, want 1", q.Len("acc1"))
	}

	// HandleTask dispatches the seeded task to the account run.
	task, ok := q.Dequeue(ctxbg(t), "acc1")
	if !ok {
		t.Fatalf("dequeue failed")
	}
	if task.Kind != queue.KindLift {
		t.Fatalf("task kind = %q", task.Kind)
	}
	if err := f.HandleTask(context.Background(), task); err != nil {
		t.Fatalf("HandleTask: %v", err)
	}
	if h.fake.touch != 1 {
		t.Fatalf("touch calls = %d, want 1 (task executed the lift)", h.fake.touch)
	}
}

func TestScheduleTimesForPerDay(t *testing.T) {
	got := scheduleTimes(6)
	want := []string{"00:00", "04:00", "08:00", "12:00", "16:00", "20:00"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("times = %v, want %v", got, want)
	}
	if got := scheduleTimes(3); len(got) != 3 || got[0] != "00:00" || got[2] != "16:00" {
		t.Fatalf("times(3) = %v", got)
	}
	if got := scheduleTimes(0); len(got) != 6 {
		t.Fatalf("times(0) = %v, want default 6", got)
	}
}

func TestLoggedOutStopsAccountAndFlagsReview(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	if err := h.db.AccountsUpsert(context.Background(), store.Account{ID: "acc1", Status: "ok"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	h.fake.touchErr = func(int) error {
		return &touchError{redirect: "/account/login?backurl=%2Fapplicant%2Fresumes"}
	}
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	acc, _ := h.db.AccountsGet(context.Background(), "acc1")
	if acc == nil || acc.Status != "logged_out" {
		t.Fatalf("account status = %+v, want logged_out", acc)
	}
	if open, _ := h.db.ReviewItemsOpen(context.Background(), "acc1", 10); len(open) == 0 {
		t.Fatalf("want a review item for logged-out")
	}
}

func ctxbg(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
