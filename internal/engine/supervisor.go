package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/audit"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/features"
	"hhauto/internal/hhapi"
	"hhauto/internal/proxy"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// webhookEndpointList materializes the [webhook.<id>] map into a
// URL-sorted slice (deterministic redeliverer construction).
func webhookEndpointList(m map[string]config.Webhook) []config.Webhook {
	urls := make([]string, 0, len(m))
	byURL := make(map[string]config.Webhook, len(m))
	for _, w := range m {
		if w.URL == "" {
			continue
		}
		if _, seen := byURL[w.URL]; !seen {
			urls = append(urls, w.URL)
		}
		byURL[w.URL] = w
	}
	sort.Strings(urls)
	out := make([]config.Webhook, 0, len(urls))
	for _, u := range urls {
		out = append(out, byURL[u])
	}
	return out
}

// Tuning constants. The 30s/60s ticks and the 15m session TTL are the
// documented engine cadence (docs/PLAN.md §Engine); the backoff ceilings
// keep a broken account from hammering hh.ru while a human is needed.
const (
	SchedulerTick = 30 * time.Second
	OutboxTick    = 60 * time.Second
	DrainTimeout  = 30 * time.Second
	BootstrapTTL  = 15 * time.Minute

	logoutPause          = 30 * time.Minute
	rateLimitBackoffBase = 30 * time.Second
	rateLimitBackoffMax  = 15 * time.Minute
	killSwitchPoll       = 500 * time.Millisecond
	shutdownStepTimeout  = 5 * time.Second

	schedulesDueLimit   = 100
	materializeLimit    = 10000
	outboxDueLimit      = 50
	defaultMaxCatchupHr = 6
)

// Deps is the engine's whole environment, assembled by the CLI (T13):
// config, persistence, queue, feature registry, integrations and the
// per-account API clients. Nil-able fields fall back to safe defaults —
// see Run.
type Deps struct {
	Cfg      *config.Config
	DB       *store.DB
	Queue    queue.Queue
	Feats    *features.Registry
	Webhooks webhook.Notifier
	Log      *slog.Logger

	CookieStore cookies.Store
	ProxyReg    proxy.Registry
	Captcha     captcha.Resolver
	AIRunner    ai.Runner

	// APIs maps account ID → client (BuildAPIClients builds it). Accounts
	// without an entry skip session checks and fail their tasks loudly.
	APIs map[string]hhapi.HHAPI

	// Optional overrides; nil selects engine defaults.
	KillSwitch KillSwitch
	Pacer      Pacer
	Outbox     webhook.Outbox // nil disables the outbox redelivery worker
	Clock      Clock
	// Rnd is the process randomness seed; Run derives isolated sources from
	// it (a *rand.Rand is not goroutine-safe, so consumers never share one).
	Rnd *rand.Rand
}

// Run drives the engine until ctx is cancelled, a kill-switch channel
// trips, or a fatal startup error occurs. Shutdown order (documented
// graceful sequence): stop the scheduler → drain the queue (up to 30s) →
// stop workers → flush cookies → close webhooks → close the store.
func Run(ctx context.Context, deps Deps) error {
	if deps.Cfg == nil {
		return errors.New("engine: Deps.Cfg is nil")
	}
	if deps.DB == nil {
		return errors.New("engine: Deps.DB is nil")
	}
	if deps.Queue == nil {
		return errors.New("engine: Deps.Queue is nil")
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = RealClock()
	}
	if deps.KillSwitch == nil {
		deps.KillSwitch = NewKillSwitch(StopFilePath(deps.Cfg.General.DataDir))
	}
	if deps.Pacer == nil {
		p := NewPacer(deps.Cfg.Pacing, deps.Rnd)
		for id, acct := range deps.Cfg.Accounts {
			p.SetExpectedDaily(id, acct.DailyApplyCap)
		}
		deps.Pacer = p
	}

	// Isolated randomness per concurrent consumer (math/rand.Rand is not
	// goroutine-safe; derived before any goroutine starts).
	baseSeed := time.Now().UnixNano()
	if deps.Rnd != nil {
		baseSeed = deps.Rnd.Int63()
	}
	schedRnd := rand.New(rand.NewSource(baseSeed))

	s := &supervisor{
		deps:      deps,
		pacer:     deps.Pacer,
		rnd:       schedRnd,
		bindings:  registeredSchedules(),
		review:    review.New(deps.Webhooks, deps.Log),
		redeliver: newRedeliverer(webhookEndpointList(deps.Cfg.Webhook), deps.ProxyReg, deps.Log),
		bootstrap: map[string]time.Time{},
		pause:     map[string]time.Time{},
		backoff:   map[string]int{},
		attempts:  map[int64]int{},
	}

	// runCtx: parent + SIGINT/SIGTERM (signal channel of the kill switch).
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	sigCtx, stopSignals := signal.NotifyContext(runCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	// Kill-switch watcher: an instant notification channel when the engine
	// built the switch (file flag or programmatic Stop), a periodic poll as
	// the generic fallback; a trip begins the graceful shutdown.
	var stopCh <-chan struct{}
	if sn, ok := deps.KillSwitch.(stopNotifier); ok {
		stopCh = sn.StopCh()
	}
	ksDone := make(chan struct{})
	go func() {
		defer close(ksDone)
		for {
			select {
			case <-sigCtx.Done():
				return
			case <-stopCh:
				deps.Log.Warn("engine: kill switch tripped, shutting down")
				cancelRun()
				return
			case <-s.deps.Clock.After(killSwitchPoll):
				if deps.KillSwitch.Tripped(context.Background()) {
					deps.Log.Warn("engine: kill switch tripped, shutting down")
					cancelRun()
					return
				}
			}
		}
	}()

	if err := s.seedFeatures(sigCtx); err != nil {
		cancelRun()
		<-ksDone
		return err
	}
	if err := s.materializeSchedules(sigCtx); err != nil {
		cancelRun()
		<-ksDone
		return err
	}

	// Scheduler + outbox loops: cancelled first at shutdown (no new work).
	schedCtx, cancelSched := context.WithCancel(sigCtx)
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		s.schedulerLoop(schedCtx)
	}()
	outboxDone := make(chan struct{})
	if deps.Outbox != nil {
		go func() {
			defer close(outboxDone)
			s.outboxLoop(schedCtx)
		}()
	} else {
		close(outboxDone)
	}

	// Account supervisors: workers keep serving the drain after the
	// scheduler stopped, so their context cancels only after Close.
	workerCtx, cancelWorkers := context.WithCancel(sigCtx)
	var workerWG sync.WaitGroup
	accounts := s.sortedAccounts()
	for _, id := range accounts {
		if p, ok := s.deps.Pacer.(*pacer); ok {
			p.SetExpectedDaily(id, s.deps.Cfg.Accounts[id].DailyApplyCap)
		}
	}
	for _, id := range accounts {
		workerWG.Add(1)
		go func(accountID string) {
			defer workerWG.Done()
			s.accountLoop(workerCtx, accountID)
		}(id)
	}
	deps.Log.Info("engine: running", "accounts", len(accounts), "schedules", len(s.bindings))

	// Block until signal / kill-switch / parent ctx.
	<-sigCtx.Done()
	stopSignals()
	deps.Log.Info("engine: shutting down")

	cancelSched()
	<-schedDone
	<-outboxDone

	closeCtx, cancelClose := context.WithTimeout(context.Background(), DrainTimeout+shutdownStepTimeout)
	defer cancelClose()
	if err := deps.Queue.Close(closeCtx, DrainTimeout); err != nil {
		deps.Log.Warn("engine: queue drain", "err", err)
	}
	cancelWorkers()
	workerWG.Wait()

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), shutdownStepTimeout)
	flushCookies(flushCtx, deps.CookieStore, deps.APIs, deps.Log)
	cancelFlush()

	if deps.Webhooks != nil {
		whCtx, cancelWH := context.WithTimeout(context.Background(), shutdownStepTimeout)
		if err := deps.Webhooks.Close(whCtx); err != nil {
			deps.Log.Warn("engine: webhook close", "err", err)
		}
		cancelWH()
	}
	if err := deps.DB.Close(); err != nil {
		deps.Log.Warn("engine: db close", "err", err)
	}
	cancelRun()
	<-ksDone
	deps.Log.Info("engine: stopped")
	return nil
}

// --- account supervision ---
// supervisor owns all engine state for one Run.
type supervisor struct {
	deps     Deps
	pacer    Pacer
	rnd      *rand.Rand // scheduler-loop-only (single goroutine)
	bindings map[string]scheduleBinding

	review    *review.Service
	redeliver Redeliverer

	mu        sync.Mutex
	bootstrap map[string]time.Time // account → last successful bootstrap
	pause     map[string]time.Time // account → paused until
	backoff   map[string]int       // account → consecutive 429 level
	attempts  map[int64]int        // outbox id → redelivery attempts
}

func (s *supervisor) sortedAccounts() []string {
	ids := make([]string, 0, len(s.deps.Cfg.Accounts))
	for id := range s.deps.Cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// seedFeatures calls RegisterTasks on every enabled feature (the Feature
// contract): features enqueue seed work and add their specs to reg. Specs
// without an engine binding are reported — the wiring layer must call
// engine.RegisterSchedule for them.
func (s *supervisor) seedFeatures(ctx context.Context) error {
	if s.deps.Feats == nil {
		return nil
	}
	reg := scheduler.NewRegistry()
	for _, f := range s.deps.Feats.Enabled() {
		if err := f.RegisterTasks(ctx, s.deps.Queue, reg); err != nil {
			return fmt.Errorf("engine: feature %s: RegisterTasks: %w", f.Name(), err)
		}
	}
	for _, name := range reg.Names() {
		if _, bound := s.bindings[name]; !bound {
			s.deps.Log.Warn("engine: feature schedule has no engine binding (engine.RegisterSchedule not called); it will not fire", "schedule", name)
		}
	}
	return nil
}

// scheduleSpecJSON is the persisted row.Spec document: the schedule name
// (to find the factory after restart) plus the spec (to recompute next_run
// with fresh jitter per fire).
type scheduleSpecJSON struct {
	Name string         `json:"name"`
	Spec scheduler.Spec `json:"spec"`
}

func scheduleRowID(name, accountID string) string { return name + "@" + accountID }

func (s *supervisor) specWithDefaultTZ(spec scheduler.Spec) (scheduler.Spec, error) {
	tz := spec.TZ
	if tz == "" || strings.EqualFold(tz, "Local") {
		tz = s.deps.Cfg.General.Timezone
	}
	if tz == "" || strings.EqualFold(tz, "Local") {
		tz = "Local"
		return spec, nil
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return spec, fmt.Errorf("bad timezone %q: %w", tz, err)
	}
	spec.TZ = tz
	return spec, nil
}

// materializeSchedules persists one schedule row per (binding × account).
// next_run is computed ONCE with jitter at first materialization; rows
// already due in the store are left alone so the first tick applies the
// missed-run policy to them (store exposes no list DAO, so "already
// exists" is approximated by "is due right now" — documented deviation).
func (s *supervisor) materializeSchedules(ctx context.Context) error {
	now := s.deps.Clock.Now()
	dueRows, err := s.deps.DB.SchedulesDue(ctx, now, materializeLimit)
	if err != nil {
		return fmt.Errorf("engine: schedules due scan: %w", err)
	}
	existing := make(map[string]struct{}, len(dueRows))
	for _, r := range dueRows {
		existing[r.ID] = struct{}{}
	}
	for _, accountID := range s.sortedAccounts() {
		for _, name := range s.sortedBindingNames() {
			b := s.bindings[name]
			id := scheduleRowID(name, accountID)
			if _, ok := existing[id]; ok {
				continue
			}
			spec, err := s.specWithDefaultTZ(b.spec)
			if err != nil {
				s.deps.Log.Error("engine: schedule skipped: bad timezone", "schedule", name, "err", err)
				continue
			}
			next := spec.Next(now, s.rnd)
			doc, _ := json.Marshal(scheduleSpecJSON{Name: name, Spec: spec})
			if err := s.deps.DB.SchedulesUpsert(ctx, &store.Schedule{
				ID:        id,
				AccountID: accountID,
				Spec:      string(doc),
				Enabled:   true,
				NextRunAt: &next,
			}); err != nil {
				return fmt.Errorf("engine: materialize %s: %w", id, err)
			}
		}
	}
	return nil
}

func (s *supervisor) sortedBindingNames() []string {
	names := make([]string, 0, len(s.bindings))
	for name := range s.bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *supervisor) schedulerLoop(ctx context.Context) {
	// Register the first tick before the startup tick so the cadence is
	// anchored at loop start (and tests can deterministically await it).
	tickCh := s.deps.Clock.After(SchedulerTick)
	s.tick(ctx) // startup tick: applies the missed-run policy immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-tickCh:
			s.tick(ctx)
			tickCh = s.deps.Clock.After(SchedulerTick)
		}
	}
}

func (s *supervisor) tick(ctx context.Context) {
	now := s.deps.Clock.Now()
	due, err := s.deps.DB.SchedulesDue(ctx, now, schedulesDueLimit)
	if err != nil {
		s.deps.Log.Error("engine: schedules due", "err", err)
		return
	}
	for _, row := range due {
		s.handleDueSchedule(ctx, row, now)
	}
	// Review SLA scan rides the scheduler tick: cheap (latched) and bounded
	// by the account count.
	slaHours := s.deps.Cfg.Review.SLADefaultHours
	if slaHours <= 0 {
		slaHours = 24
	}
	if err := s.review.ScanSLA(ctx, s.deps.DB, slaHours, s.sortedAccounts(), now); err != nil {
		s.deps.Log.Error("engine: review SLA scan", "err", err)
	}
}

func (s *supervisor) handleDueSchedule(ctx context.Context, row store.Schedule, now time.Time) {
	log := s.deps.Log.With("schedule", row.ID, "account", row.AccountID)

	_, binding, bound := s.resolveRow(row)
	if !bound {
		log.Warn("engine: schedule row has no binding; disabling")
		s.disableRow(ctx, row)
		return
	}
	if _, configured := s.deps.Cfg.Accounts[row.AccountID]; !configured {
		log.Warn("engine: schedule for unknown account; disabling")
		s.disableRow(ctx, row)
		return
	}
	if row.NextRunAt == nil {
		log.Warn("engine: due schedule row without next_run; disabling")
		s.disableRow(ctx, row)
		return
	}

	late := now.Sub(*row.NextRunAt)
	maxCatchup := time.Duration(s.deps.Cfg.Schedule.MaxCatchupHours) * time.Hour
	if maxCatchup <= 0 {
		maxCatchup = defaultMaxCatchupHr * time.Hour
	}
	expired := late > maxCatchup
	if expired && s.missedRunPolicy() == "skip" {
		log.Info("engine: missed run skipped", "late", late.Round(time.Second))
		s.advanceSchedule(ctx, row, binding.spec, now)
		return
	}

	task, err := binding.factory(ctx, row.AccountID)
	if err != nil {
		// Leave the row due: the next tick retries the factory (transient
		// failures must not silently drop scheduled work).
		log.Error("engine: task factory failed; will retry next tick", "err", err)
		return
	}
	if err := s.deps.Queue.Enqueue(ctx, task); err != nil {
		log.Error("engine: enqueue failed", "err", err)
		return // row stays due: retried next tick
	}
	if expired {
		log.Info("engine: missed run executed once", "late", late.Round(time.Second))
	}
	s.advanceSchedule(ctx, row, binding.spec, now)
}

// resolveRow finds the binding for a due row: the persisted Spec JSON names
// the schedule; rows persisted before the JSON document existed fall back
// to the "name@account" ID convention.
func (s *supervisor) resolveRow(row store.Schedule) (string, scheduleBinding, bool) {
	if row.Spec != "" {
		var doc scheduleSpecJSON
		if err := json.Unmarshal([]byte(row.Spec), &doc); err == nil && doc.Name != "" {
			if b, ok := s.bindings[doc.Name]; ok {
				return doc.Name, b, true
			}
		}
	}
	if name, _, ok := strings.Cut(row.ID, "@"); ok {
		if b, bound := s.bindings[name]; bound {
			return name, b, true
		}
	}
	return "", scheduleBinding{}, false
}

func (s *supervisor) missedRunPolicy() string {
	policy := strings.ToLower(strings.TrimSpace(s.deps.Cfg.Schedule.MissedRunPolicy))
	if policy == "" {
		return "run_once"
	}
	return policy
}

func (s *supervisor) advanceSchedule(ctx context.Context, row store.Schedule, spec scheduler.Spec, now time.Time) {
	effective, err := s.specWithDefaultTZ(spec)
	if err != nil {
		effective = spec
	}
	next := effective.Next(now, s.rnd) // jitter applied ONCE per fire, then persisted
	if err := s.deps.DB.SchedulesMarkRun(ctx, row.ID, now, next); err != nil {
		s.deps.Log.Error("engine: advance schedule", "schedule", row.ID, "err", err)
	}
}

// disableRow keeps the row for audit but stops it from firing.
func (s *supervisor) disableRow(ctx context.Context, row store.Schedule) {
	row.Enabled = false
	if err := s.deps.DB.SchedulesUpsert(ctx, &row); err != nil {
		s.deps.Log.Error("engine: disable schedule", "schedule", row.ID, "err", err)
	}
}

// --- account supervision ---

func (s *supervisor) accountLoop(ctx context.Context, accountID string) {
	acct := s.deps.Cfg.Accounts[accountID]
	n := acct.Concurrency
	if n <= 0 {
		n = 1
	}
	// Sequential execution within an account (PLAN: supervisors parallel
	// across accounts, strictly in-account sequential); N workers give
	// redundancy so one stuck handler does not strand the queue drain.
	exec := make(chan struct{}, 1)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx, accountID, exec)
		}()
	}
	wg.Wait()
}

func (s *supervisor) worker(ctx context.Context, accountID string, exec chan struct{}) {
	log := s.deps.Log.With("account", accountID)
	for {
		task, ok := s.deps.Queue.Dequeue(ctx, accountID)
		if !ok {
			return
		}
		// Kill switch: checked before EVERY task — no mutation past a trip.
		if s.deps.KillSwitch.Tripped(ctx) {
			log.Warn("engine: kill switch tripped, dropping task", "task", task.ID, "kind", task.Kind)
			continue
		}
		// Account paused (logged out / rate limited): park the task and
		// wait out the pause instead of burning attempts.
		if d := s.pauseRemaining(accountID, s.deps.Clock.Now()); d > 0 {
			_ = s.requeue(ctx, task)
			sleepClock(ctx, s.deps.Clock, d)
			continue
		}
		if err := s.ensureSession(ctx, accountID); err != nil {
			if errors.Is(err, hhapi.ErrLoggedOut) {
				s.handleLoggedOut(ctx, accountID, err)
				_ = s.requeue(ctx, task)
				s.pauseFor(logoutPause, accountID)
				continue
			}
			// Bootstrap infrastructure errors must not block tasks: the
			// task's own calls surface typed errors with full context.
			log.Warn("engine: session bootstrap failed; proceeding", "err", err)
		}

		// One task at a time within the account.
		select {
		case exec <- struct{}{}:
		case <-ctx.Done():
			_ = s.requeue(ctx, task)
			return
		}
		err := s.dispatch(ctx, accountID, task)
		<-exec

		switch {
		case err == nil:
			s.resetBackoff(accountID)
		case errors.Is(err, hhapi.ErrRateLimited):
			s.handleRateLimit(ctx, accountID, task, log)
		default:
			var ambiguous *hhapi.AmbiguousError
			if errors.As(err, &ambiguous) {
				// Repetition is forbidden for ambiguous mutations; the task
				// is failed (features reconcile by reading state).
				log.Error("engine: ambiguous outcome; task failed (reconcile by reading)", "task", task.ID, "err", err)
			} else {
				log.Error("engine: task failed", "task", task.ID, "kind", task.Kind, "err", err)
			}
			_ = audit.Incr(ctx, s.deps.DB, audit.KeyTasksFailed)
		}
	}
}

func (s *supervisor) requeue(ctx context.Context, task queue.Task) error {
	// Re-enqueue with the same dedup key: parked work survives pauses and
	// shutdown drains (ErrClosed during shutdown means the task is lost —
	// recovery is the scheduler's job by contract).
	return s.deps.Queue.Enqueue(ctx, task)
}

// dispatch routes one task to its registered handler with panic isolation.
func (s *supervisor) dispatch(ctx context.Context, accountID string, task queue.Task) (err error) {
	h, ok := registeredHandler(task.Kind)
	if !ok {
		err := fmt.Errorf("engine: no handler registered for kind %q", task.Kind)
		s.deps.Log.Error("engine: task dropped", "task", task.ID, "err", err)
		_ = audit.Incr(ctx, s.deps.DB, audit.KeyTasksFailed)
		return err
	}
	tc := &TaskContext{
		DB:        s.deps.DB,
		API:       s.deps.APIs[accountID],
		AI:        s.deps.AIRunner,
		Pacer:     s.pacer,
		Webhooks:  s.deps.Webhooks,
		Captcha:   s.deps.Captcha,
		Cfg:       s.deps.Cfg,
		Log:       s.deps.Log,
		AccountID: accountID,
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("engine: handler panic: %v", r)
			s.deps.Log.Error("engine: handler panic recovered; worker survives",
				"account", accountID, "task", task.ID, "kind", task.Kind, "panic", r)
		}
	}()
	return h(ctx, tc, task)
}

// ensureSession runs the light authed check at most once per BootstrapTTL.
func (s *supervisor) ensureSession(ctx context.Context, accountID string) error {
	now := s.deps.Clock.Now()
	s.mu.Lock()
	last, seen := s.bootstrap[accountID]
	s.mu.Unlock()
	if seen && now.Sub(last) < BootstrapTTL {
		return nil
	}
	api, ok := s.deps.APIs[accountID]
	if !ok || api == nil {
		return fmt.Errorf("engine: no API client for account %q", accountID)
	}
	err := api.BootstrapSession(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.bootstrap[accountID] = now
	s.mu.Unlock()
	return nil
}

// handleLoggedOut marks the account, opens a (idempotent) review item and
// lets the caller pause the account until a human refreshes the session.
func (s *supervisor) handleLoggedOut(ctx context.Context, accountID string, cause error) {
	log := s.deps.Log.With("account", accountID)
	if err := s.deps.DB.AccountsSetStatus(ctx, accountID, "logged_out"); err != nil {
		log.Error("engine: mark logged_out", "err", err)
	}
	if _, err := s.review.Add(ctx, s.deps.DB, accountID, "session_expired",
		fmt.Sprintf("session bootstrap failed: %v", cause),
		fmt.Sprintf(`{"target":%q}`, accountID+":session")); err != nil {
		log.Error("engine: review add", "err", err)
	}
	log.Warn("engine: account paused: session logged out (needs human)")
}

func (s *supervisor) handleRateLimit(ctx context.Context, accountID string, task queue.Task, log *slog.Logger) {
	s.mu.Lock()
	level := s.backoff[accountID]
	s.backoff[accountID] = level + 1
	s.mu.Unlock()
	_ = audit.Incr(ctx, s.deps.DB, audit.KeyHTTP429)
	_ = s.requeue(ctx, task)
	s.pauseFor(backoffDelay(level), accountID)
	log.Warn("engine: 429 rate limited; task requeued with backoff",
		"task", task.ID, "level", level, "pause", backoffDelay(level).Round(time.Second))
}

func (s *supervisor) resetBackoff(accountID string) {
	s.mu.Lock()
	delete(s.backoff, accountID)
	s.mu.Unlock()
}

func backoffDelay(level int) time.Duration {
	d := rateLimitBackoffBase << uint(level)
	if d <= 0 || d > rateLimitBackoffMax {
		return rateLimitBackoffMax
	}
	return d
}

func (s *supervisor) pauseFor(d time.Duration, accountID string) {
	s.mu.Lock()
	s.pause[accountID] = s.deps.Clock.Now().Add(d)
	s.mu.Unlock()
}

func (s *supervisor) pauseRemaining(accountID string, now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.pause[accountID]
	if !ok || !until.After(now) {
		return 0
	}
	return until.Sub(now)
}

// --- outbox redelivery ---

func (s *supervisor) outboxLoop(ctx context.Context) {
	s.outboxTick(ctx) // tick-first: parked items are replayed at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.deps.Clock.After(OutboxTick):
			s.outboxTick(ctx)
		}
	}
}

func (s *supervisor) outboxTick(ctx context.Context) {
	items, err := s.deps.Outbox.Due(s.deps.Clock.Now(), outboxDueLimit)
	if err != nil {
		s.deps.Log.Error("engine: outbox due", "err", err)
		return
	}
	for _, item := range items {
		s.redeliverOne(ctx, item)
	}
}

func (s *supervisor) redeliverOne(ctx context.Context, item webhook.OutboxItem) {
	s.mu.Lock()
	s.attempts[item.ID]++
	attempt := s.attempts[item.ID]
	s.mu.Unlock()

	if err := s.redeliver.Redeliver(ctx, item); err != nil {
		delay := outboxBackoff(attempt)
		if merr := s.deps.Outbox.MarkFailed(item.ID, s.deps.Clock.Now().Add(delay)); merr != nil {
			s.deps.Log.Error("engine: outbox mark failed", "id", item.ID, "err", merr)
		}
		s.deps.Log.Warn("engine: outbox redelivery failed; retrying with backoff",
			"id", item.ID, "endpoint", item.EndpointID, "attempt", attempt, "next", delay.Round(time.Second), "err", err)
		return
	}
	if err := s.deps.Outbox.MarkDelivered(item.ID); err != nil {
		s.deps.Log.Error("engine: outbox mark delivered", "id", item.ID, "err", err)
		return
	}
	s.mu.Lock()
	delete(s.attempts, item.ID)
	s.mu.Unlock()
	s.deps.Log.Info("engine: outbox redelivered", "id", item.ID, "endpoint", item.EndpointID, "attempt", attempt)
}

// outboxBackoff: exponential with a 1h ceiling; a permanently broken
// endpoint is retried hourly (poison items are never dropped — webhook
// delivery is best-effort by contract, but the payload is the operator's).
func outboxBackoff(attempt int) time.Duration {
	const base = time.Minute
	const max = time.Hour
	d := base << uint(attempt-1)
	if d <= 0 || d > max {
		return max
	}
	return d
}
