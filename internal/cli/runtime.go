package cli

// Runtime is the wired application environment shared by every command:
// config, persistence, integrations, the feature registry and the engine
// bindings. Build assembles it in the documented order (config → store →
// cookies → proxy → captcha → ai → webhook → api → profile → pacer →
// kill-switch → queue → features → registration); Close tears it down in
// reverse.
//
// CLI glue defaults (applied to the in-memory config copy before any
// feature is constructed):
//   - general.data_dir empty        → ~/.local/share/hhauto
//   - storage.path empty            → <data_dir>/hhauto.db
//   - cookie storage path empty     → <data_dir>/cookies
//   - cookie backend                → auto; the HHAUTO_COOKIES_BACKEND env
//     ("keyring" | "encrypted-file") pins it explicitly (ops/test seam; the
//     TOML config carries no cookie knobs by contract).

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/browser"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/engine"
	"hhauto/internal/features"
	"hhauto/internal/features/autolift"
	"hhauto/internal/features/chatops"
	"hhauto/internal/features/contacts"
	"hhauto/internal/features/emaildup"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/kit11"
	"hhauto/internal/features/multiapply"
	"hhauto/internal/features/testsolve"
	"hhauto/internal/features/warmup"
	"hhauto/internal/hhapi"
	"hhauto/internal/logging"
	"hhauto/internal/profile"
	"hhauto/internal/proxy"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// featureFilter maps --feature names onto their config enable flags. Names
// are the stable feature identifiers (features.Name()). The warmup toggle
// lives in the top-level [warmup] section, not under [features].
var featureFilters = map[string]func(c *config.Config) *bool{
	"autolift":   func(c *config.Config) *bool { return &c.Features.AutoLift.Enabled },
	"multiapply": func(c *config.Config) *bool { return &c.Features.MultiApply.Enabled },
	"chatops":    func(c *config.Config) *bool { return &c.Features.ChatOps.Enabled },
	"testsolve":  func(c *config.Config) *bool { return &c.Features.TestSolve.Enabled },
	"contacts":   func(c *config.Config) *bool { return &c.Features.Contacts.Enabled },
	"warmup":     func(c *config.Config) *bool { return &c.Warmup.Enabled },
	"emaildup":   func(c *config.Config) *bool { return &c.Features.EmailDupEnabled },
}

// BuildOptions selects what Build loads. Zero values mean "no filtering".
type BuildOptions struct {
	ConfigPath string
	DryRun     bool   // --dry-run: force general.dry_run in the config copy
	Account    string // --account: restrict config to this account
	Feature    string // --feature: restrict the registry to this feature
	Profile    string // --profile: restrict runs to this run id
	LogLevel   string // --log-level: overrides log.level
}

// schedBinding records one engine.RegisterSchedule binding so status can
// compute next-run times without touching the store.
type schedBinding struct {
	Name    string
	Spec    scheduler.Spec
	Factory engine.TaskFactory
}

// Runtime is the wired application.
type Runtime struct {
	Cfg *config.Config
	DB  *store.DB
	Log *slog.Logger

	closeLog func() error

	Cookies  cookies.Store
	ProxyReg proxy.Registry
	Captcha  captcha.Resolver // nil when no providers are configured
	AI       ai.Runner        // nil when no ai.tasks are configured
	Webhooks webhook.Notifier
	Outbox   webhook.Outbox
	Queue    queue.Queue
	Pacer    engine.Pacer
	Kill     engine.KillSwitch

	Feats *features.Registry

	review *review.Service // review-queue fan-out source for features and TaskContext

	schedules []schedBinding
	handlers  map[queue.TaskKind]engine.TaskHandler

	apis    map[string]hhapi.HHAPI
	apisOne sync.Once
	apisErr error

	ownsShutdown bool // engine.Run already closed db/webhooks
	closed       bool
}

// LoadConfigOnly reads and validates the config without opening anything
// (config validate/template, cheap checks).
func LoadConfigOnly(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	applyCLIDefaults(cfg)
	return cfg, nil
}

// applyCLIDefaults fills the glue defaults the keystone owns. The config
// package intentionally leaves these unset (they are deployment paths, not
// feature contracts); an empty one would fail downstream with a less clear
// error.
func applyCLIDefaults(c *config.Config) {
	if c.General.DataDir == "" {
		c.General.DataDir = config.ExpandPath("~/.local/share/hhauto")
	}
	if c.Storage.Path == "" {
		c.Storage.Path = filepath.Join(c.General.DataDir, "hhauto.db")
	}
}

// applyFilters narrows the config copy per --account/--profile/--feature.
func applyFilters(c *config.Config, opts BuildOptions) error {
	if opts.Account != "" {
		acct, ok := c.Accounts[opts.Account]
		if !ok {
			return fmt.Errorf("account %q is not configured", opts.Account)
		}
		c.Accounts = map[string]config.Account{opts.Account: acct}
		for id, r := range c.Runs {
			if r.Enabled && r.Account != opts.Account {
				r.Enabled = false
				c.Runs[id] = r
			}
		}
	}
	if opts.Profile != "" {
		run, ok := c.Runs[opts.Profile]
		if !ok {
			return fmt.Errorf("run profile %q is not configured", opts.Profile)
		}
		c.Runs = map[string]config.Run{opts.Profile: run}
	}
	if opts.Feature != "" {
		if _, ok := featureFilters[opts.Feature]; !ok {
			return fmt.Errorf("unknown feature %q (want one of: %s)",
				opts.Feature, strings.Join(sortedFeatureNames(), ", "))
		}
		for name, get := range featureFilters {
			if name != opts.Feature {
				*get(c) = false
			}
		}
	}
	return nil
}

func sortedFeatureNames() []string {
	names := make([]string, 0, len(featureFilters))
	for n := range featureFilters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Build wires the full runtime. Everything is fail-fast: a broken
// environment returns an error instead of a half-usable Runtime.
func Build(ctx context.Context, opts BuildOptions) (*Runtime, error) {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	applyCLIDefaults(cfg)

	if opts.DryRun {
		cfg.General.DryRun = true // BEFORE feature construction: every feature reads this flag
	}
	if opts.LogLevel != "" {
		cfg.Log.Level = opts.LogLevel
	}
	if err := applyFilters(cfg, opts); err != nil {
		return nil, err
	}

	rt := &Runtime{Cfg: cfg, handlers: map[queue.TaskKind]engine.TaskHandler{}}
	fail := func(err error) (*Runtime, error) {
		_ = rt.Close()
		return nil, err
	}

	rt.Log, rt.closeLog = logging.New(cfg.Log)
	if rt.Log == nil {
		return fail(fmt.Errorf("logging: nil logger"))
	}

	if err := os.MkdirAll(cfg.General.DataDir, 0o750); err != nil {
		return fail(fmt.Errorf("create data dir: %w", err))
	}
	rt.DB, err = store.Open(ctx, cfg.Storage.Path, store.Options{BusyTimeoutMS: cfg.Storage.BusyTimeoutMS})
	if err != nil {
		return fail(err)
	}

	cookieStore, err := resolveCookieStore(cfg)
	if err != nil {
		return fail(fmt.Errorf("cookie store: %w", err))
	}
	rt.Cookies = cookieStore
	rt.ProxyReg, err = proxy.NewRegistry(cfg.Proxy)
	if err != nil {
		return fail(fmt.Errorf("proxy registry: %w", err))
	}

	// Captcha: absent [captcha] section is a valid "captcha disabled" setup;
	// a configured-but-broken provider table is fatal (fail fast).
	if len(cfg.Captcha) > 0 {
		chainFor := func(accountID string) []string { return cfg.Accounts[accountID].CaptchaChain }
		rt.Captcha, err = captcha.New(cfg.Captcha, chainFor, rt.Log)
		if err != nil {
			return fail(fmt.Errorf("captcha resolver: %w", err))
		}
	}

	rt.AI, err = buildAI(cfg, rt.DB, rt.Log)
	if err != nil {
		return fail(err)
	}

	rt.Outbox = newStoreOutbox(rt.DB)
	rt.Webhooks = webhook.New(webhookEndpointConfigs(cfg), rt.ProxyReg, rt.Outbox, rt.Log)
	rt.review = review.New(rt.Webhooks, rt.Log)

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	pacer := engine.NewPacer(cfg.Pacing, rnd)
	for id, acct := range cfg.Accounts {
		pacer.SetExpectedDaily(id, acct.DailyApplyCap)
	}
	rt.Pacer = pacer
	rt.Kill = engine.NewKillSwitch(engine.StopFilePath(cfg.General.DataDir))
	rt.Queue = queue.New()

	if err := rt.buildFeatures(); err != nil {
		return fail(err)
	}
	if err := rt.registerWithEngine(ctx); err != nil {
		return fail(err)
	}
	return rt, nil
}

// webhookEndpointConfigs materializes the [webhook.<id>] map into a
// URL-sorted slice (deterministic notifier construction; the engine keeps
// its own copy for redelivery).
func webhookEndpointConfigs(cfg *config.Config) []config.Webhook {
	urls := make([]string, 0, len(cfg.Webhook))
	byURL := make(map[string]config.Webhook, len(cfg.Webhook))
	for _, w := range cfg.Webhook {
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

// contentEditorFor resolves the per-account resume content surface for
// the improve pipeline and the browser editor runner (type-asserted off
// the full client; an account whose client lacks the surface fails here).
func (rt *Runtime) contentEditorFor(accountID string) (contentEditorAPI, error) {
	api, err := rt.APIFor(accountID)
	if err != nil {
		return nil, err
	}
	ed, ok := api.(contentEditorAPI)
	if !ok {
		return nil, fmt.Errorf("cli: api client for account %q does not implement the resume content editor", accountID)
	}
	return ed, nil
}

// buildFeatures constructs every feature module with its dependencies and
// fills the registry. Disabled features are constructed too (they are
// inert; the engine drives Enabled() only) so the registry stays complete.
func (rt *Runtime) buildFeatures() error {
	cfg := rt.Cfg
	apiFor := featurekit.APIFor(rt.APIFor)
	chainFor := featurekit.ChainFor(rt.chainFor)
	dialerFor := rt.dialerFor

	feats := &features.Registry{}
	reviewer := rt.review.For(rt.DB)
	stopped := func(ctx context.Context) bool { return rt.Kill != nil && rt.Kill.Tripped(ctx) }
	lift := autolift.New(autolift.Options{
		Config: *cfg, DB: rt.DB, APIFor: apiFor, ChainFor: chainFor,
		Pacer: rt.Pacer, Log: rt.Log,
	})
	feats.Add(lift)
	improver, err := buildImprove(cfg, rt.AI, rt.DB, rt.contentEditorFor, rt.Cookies, rt.Log)
	if err != nil {
		return err
	}
	apply := multiapply.New(multiapply.Options{
		Config: *cfg, DB: rt.DB, APIFor: apiFor, ChainFor: chainFor,
		Runner: rt.AI, ProfileFor: rt.profileFor, Pacer: rt.Pacer,
		Notifier: rt.Webhooks, Review: reviewer, Stopped: stopped, Log: rt.Log,
		Improver: improver,
	})
	feats.Add(apply)
	chat := chatops.New(*cfg, rt.DB, rt.AI, rt.Webhooks, kit11.APIResolver(rt.APIFor), rt.Log)
	chat.Review = reviewer
	chat.Stopped = stopped
	feats.Add(chat)
	solver := &testsolve.Solver{
		Cfg: *cfg, DB: rt.DB, AI: rt.AI, Notify: rt.Webhooks,
		Dialer: dialerFor, Log: rt.Log,
		Review: reviewer, Stopped: stopped,
		ChromeProvider: browser.NewFromConfig(cfg.Browser, rt.Log),
	}
	feats.Add(solver)
	cont := &contacts.Feature{
		Cfg: *cfg, DB: rt.DB, AI: rt.AI, API: kit11.APIResolver(rt.APIFor),
		Notify: rt.Webhooks, Log: rt.Log,
	}
	feats.Add(cont)
	warm := &warmup.Feature{
		Cfg: *cfg, DB: rt.DB, AI: rt.AI, API: kit11.APIResolver(rt.APIFor),
		Notify: rt.Webhooks, Log: rt.Log,
		Review: reviewer,
	}
	feats.Add(warm)
	mail := &emaildup.Feature{
		Cfg: *cfg, DB: rt.DB, API: kit11.APIResolver(rt.APIFor), Notify: rt.Webhooks,
		Log: rt.Log, Dialer: dialerFor,
	}
	feats.Add(mail)

	rt.Feats = feats
	rt.registerFeatureHandlers(lift, apply, chat, solver, cont, warm, mail)
	return nil
}

// registerFeatureHandlers binds task kinds to their executors, only for
// enabled features (disabled features never enqueue; a registered handler
// for them would be dead code). Each closure closes over the constructed
// feature; the engine's TaskContext mirrors the wiring objects and is
// accepted for the contract (features hold their own deps).
func (rt *Runtime) registerFeatureHandlers(lift *autolift.Feature, apply *multiapply.Feature,
	chat *chatops.Feature, solver *testsolve.Solver, cont *contacts.Feature,
	warm *warmup.Feature, mail *emaildup.Feature) {
	set := func(kind queue.TaskKind, fn engine.TaskHandler) {
		rt.handlers[kind] = fn
	}
	if rt.Cfg.Features.AutoLift.Enabled {
		set(queue.KindLift, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return lift.HandleTask(ctx, task)
		})
	}
	if rt.Cfg.Features.MultiApply.Enabled {
		set(queue.KindApply, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return apply.HandleTask(ctx, task)
		})
	}
	if rt.Cfg.Features.ChatOps.Enabled {
		set(queue.KindChatSync, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return chat.HandleChatSync(ctx, task)
		})
	}
	if rt.Cfg.Features.TestSolve.Enabled {
		set(queue.KindSolveTest, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return solver.HandleSolveTest(ctx, task)
		})
	}
	if rt.Cfg.Features.Contacts.Enabled {
		set(queue.KindCollectContacts, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return cont.HandleCollectContacts(ctx, task)
		})
	}
	if rt.Cfg.Warmup.Enabled {
		// warmup consumes KindWarmupPing; its enabled state is the
		// top-level [warmup] toggle.
		set(queue.KindWarmupPing, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return warm.HandleScan(ctx, task)
		})
	}
	if rt.Cfg.Features.EmailDupEnabled {
		set(queue.KindSendEmail, func(ctx context.Context, _ *engine.TaskContext, task queue.Task) error {
			return mail.HandleSendEmail(ctx, task)
		})
	}
}

// registerWithEngine installs the engine bindings: one RegisterHandler per
// enabled feature kind and one RegisterSchedule per feature-registered spec
// with a name-mapped task factory. The CLI performs its own RegisterTasks
// pass (seeding is dedup-idempotent; engine.Run repeats it harmlessly) to
// learn each feature's spec names.
func (rt *Runtime) registerWithEngine(ctx context.Context) error {
	for kind, h := range rt.handlers {
		if err := engine.RegisterHandler(kind, h); err != nil {
			return fmt.Errorf("register handler %s: %w", kind, err)
		}
	}

	reg := scheduler.NewRegistry()
	for _, f := range rt.Feats.Enabled() {
		if err := f.RegisterTasks(ctx, rt.Queue, reg); err != nil {
			return fmt.Errorf("seed feature %s: %w", f.Name(), err)
		}
	}
	for _, name := range reg.Names() {
		spec, _ := reg.Get(name)
		// Feature packages may build Spec{} literals without a weekday
		// mask (chatops/warmup interval specs); scheduler.Spec.Next treats
		// Days=0 as "no day matches", so normalize to every-day here —
		// the same default scheduler.Parse applies.
		if spec.Days == 0 {
			spec.Days = scheduler.All
		}
		factory := scheduleFactory(name, rt)
		if factory == nil {
			rt.Log.Warn("cli: no task factory for feature schedule; it will not fire", "schedule", name)
			continue
		}
		if err := engine.RegisterSchedule(name, spec, factory); err != nil {
			return fmt.Errorf("register schedule %s: %w", name, err)
		}
		rt.schedules = append(rt.schedules, schedBinding{Name: name, Spec: spec, Factory: factory})
	}
	return nil
}

// scheduleFactory maps a feature-registered schedule name onto the task the
// engine must enqueue when it fires. Names live in the feature packages;
// the engine reports any drift ("schedule has no binding") loudly.
func scheduleFactory(name string, rt *Runtime) engine.TaskFactory {
	now := func() time.Time { return time.Now() }
	switch {
	case name == "features.autolift":
		return func(_ context.Context, accountID string) (queue.Task, error) {
			window := featurekit.LiftWindow(now())
			return queue.Task{
				AccountID: accountID, Kind: queue.KindLift,
				DedupKey: "lift:" + window, Priority: queue.PrioLift,
				Deadline: now().Add(4 * time.Hour),
				Payload:  autolift.LiftPayload{AccountID: accountID, Window: window},
			}, nil
		}
	case name == "chatops.poll":
		return func(_ context.Context, accountID string) (queue.Task, error) {
			return queue.Task{
				AccountID: accountID, Kind: queue.KindChatSync,
				Priority: queue.PrioChats, DedupKey: "chatops-poll",
				Payload: chatops.SyncPayload{AccountID: accountID},
			}, nil
		}
	case name == "warmup.scan":
		return func(_ context.Context, accountID string) (queue.Task, error) {
			return queue.Task{
				AccountID: accountID, Kind: queue.KindWarmupPing,
				Priority: queue.PrioWarmup, DedupKey: "warmup-scan",
				Payload: warmup.ScanPayload{AccountID: accountID},
			}, nil
		}
	case len(name) > 5 && name[:5] == "runs.":
		runID := name[5:]
		run, ok := rt.Cfg.Runs[runID]
		if !ok {
			return nil
		}
		return func(_ context.Context, _ string) (queue.Task, error) {
			return queue.Task{
				AccountID: run.Account, Kind: queue.KindApply, RunID: runID,
				DedupKey: "multiapply:" + runID, Priority: queue.PrioApply,
				Payload: multiapply.RunPayload{RunID: runID},
			}, nil
		}
	default:
		return nil
	}
}

// AllAPIs builds (once) and returns the whole per-account client map via
// engine.BuildAPIClients — proxy dialers, cookie seeding, response
// persistence. Used by `run` to hand the engine its supervision map.
func (rt *Runtime) AllAPIs() (map[string]hhapi.HHAPI, error) {
	rt.apisOne.Do(func() {
		apis, err := engine.BuildAPIClients(context.Background(), rt.Cfg, rt.ProxyReg, rt.Cookies, rt.Log)
		if err != nil {
			rt.apisErr = err
			return
		}
		rt.apis = apis
	})
	return rt.apis, rt.apisErr
}

// APIFor resolves the per-account hh client lazily: the map is built once
// on first use through engine.BuildAPIClients and cached.
func (rt *Runtime) APIFor(accountID string) (hhapi.HHAPI, error) {
	apis, err := rt.AllAPIs()
	if err != nil {
		return nil, err
	}
	api, ok := apis[accountID]
	if !ok {
		return nil, fmt.Errorf("cli: no api client for account %q", accountID)
	}
	return api, nil
}

// chainFor adapts the captcha resolver onto featurekit.ChainFor: ok=false
// means no chain is configured (captcha failures are terminal for the task).
func (rt *Runtime) chainFor(accountID string) (captcha.Chain, bool) {
	if rt.Captcha == nil {
		return nil, false
	}
	chain, err := rt.Captcha.ChainFor(accountID)
	if err != nil {
		rt.Log.Debug("cli: no captcha chain", "account", accountID, "err", err)
		return nil, false
	}
	return chain, true
}

// dialerFor resolves the account proxy dialer for the fetch-based features
// (testsolve, emaildup). A nil dialer with a nil error means explicitly
// direct; an unknown ref fails.
func (rt *Runtime) dialerFor(accountID string) (proxy.Dialer, error) {
	acct, ok := rt.Cfg.Accounts[accountID]
	if !ok || acct.ProxyRef == "" {
		return nil, nil
	}
	if rt.ProxyReg == nil {
		return nil, fmt.Errorf("cli: proxy_ref %q set but no proxy registry", acct.ProxyRef)
	}
	return rt.ProxyReg.Dialer(acct.ProxyRef)
}

// profileFor builds the per-account trusted profile context through
// internal/profile (resume snapshot + profile.md + stored facts). The
// accounts row is ensured first: resumes.account_id carries a FOREIGN KEY
// to accounts(id) and the profile write-through inserts resume rows.
func (rt *Runtime) profileFor(ctx context.Context, accountID string) (profile.Profile, error) {
	api, err := rt.APIFor(accountID)
	if err != nil {
		return profile.Profile{}, err
	}
	if err := rt.DB.AccountsUpsert(ctx, store.Account{ID: accountID}); err != nil {
		return profile.Profile{}, fmt.Errorf("ensure account row: %w", err)
	}
	return profile.LoadUserContext(ctx, *rt.Cfg, accountID, rt.DB, api)
}

// TaskContext builds the engine task context for a manual dispatch (--once).
func (rt *Runtime) TaskContext(accountID string) (*engine.TaskContext, error) {
	api, err := rt.APIFor(accountID)
	if err != nil {
		return nil, err
	}
	return &engine.TaskContext{
		DB: rt.DB, API: api, AI: rt.AI, Pacer: rt.Pacer,
		Webhooks: rt.Webhooks, Captcha: rt.Captcha,
		Cfg: rt.Cfg, Log: rt.Log, AccountID: accountID,
		Review:  rt.review.For(rt.DB),
		Stopped: rt.Kill.Tripped,
	}, nil
}

// Handlers exposes the cli-owned handler map (the --once runner dispatches
// through it; the engine uses its own registry fed by registerWithEngine).
func (rt *Runtime) Handlers() map[queue.TaskKind]engine.TaskHandler { return rt.handlers }

// Schedules exposes the registered schedule bindings (status next-run).
//
//nolint:revive // sealed impl: schedBinding is a cli-internal wiring record; consumers live in this package
func (rt *Runtime) Schedules() []schedBinding { return rt.schedules }

// MarkEngineOwned records that engine.Run performed the graceful shutdown
// (queue drain, cookie flush, webhook close, db close) — Close skips those.
func (rt *Runtime) MarkEngineOwned() { rt.ownsShutdown = true }

// Close tears the runtime down. Idempotent; safe after engine.Run.
// closeQueueAndWebhooks drains the queue and closes webhooks with short
// timeouts — only when the runtime is NOT engine-owned (engine.Run performs
// its own ordered shutdown of the same resources).
func (rt *Runtime) closeQueueAndWebhooks(firstErr *error) {
	if rt.ownsShutdown {
		return
	}
	if rt.Queue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := rt.Queue.Close(ctx, time.Second); err != nil && *firstErr == nil {
			*firstErr = err
		}
		cancel()
	}
	if rt.Webhooks != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := rt.Webhooks.Close(ctx); err != nil && *firstErr == nil {
			*firstErr = err
		}
		cancel()
	}
}

// Close tears the runtime down. Idempotent; safe after engine.Run.
func (rt *Runtime) Close() error {
	if rt == nil || rt.closed {
		return nil
	}
	rt.closed = true
	var firstErr error
	rt.closeQueueAndWebhooks(&firstErr)
	if rt.DB != nil {
		if err := rt.DB.Close(); err != nil && !rt.ownsShutdown && firstErr == nil {
			firstErr = err
		}
	}
	if rt.closeLog != nil {
		_ = rt.closeLog()
	}
	return firstErr
}
