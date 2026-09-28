package e2e

// The e2e harness: a hermetic hhauto.toml pointed at the fake hh + fake AI
// servers, the full cli.Build wiring, and engine.Run driven by a RealClock
// fake (real time with a controllable shift). Scenarios observe outcomes by
// polling the store and the fakes through waitUntil — no sleep-based
// expectations anywhere: every wait ends exactly when the expected
// end-state becomes visible, or fails by deadline.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hhauto/internal/audit"
	"hhauto/internal/cli"
	"hhauto/internal/engine"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/store"
	"hhauto/internal/testutil/tlsfix"
)

// testClock implements engine.Clock over real time with an injectable
// offset: the engine loops get a deterministic handle on "now", the After
// channel stays real (the engine only needs it for ticks and polls that the
// scenarios never wait on).
type testClock struct {
	offset atomic.Int64 // nanoseconds
}

func (c *testClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *testClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Advance shifts the engine clock forward without sleeping.
func (c *testClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

// e2eEnv is one fully wired scenario environment.
type e2eEnv struct {
	t       *testing.T
	hh      *fakeHH
	ai      *fakeAI
	clock   *testClock
	cfgPath string
	dataDir string
	db      *store.DB // harness-side handle: seeding + polling reads
}

// envOptions carries the per-scenario config deltas. Everything else stays
// identical across scenarios so the process-global engine registry only
// ever sees the same schedule names (re-registration replaces; no stale
// bindings leak between tests).
type envOptions struct {
	dryRun        bool
	chatsOff      bool  // disable chatops (kill-switch scenario: the only mutations are applies)
	warmupSilence int   // hours; 0 → default 10000 (never due)
	warmupLadder  []int // 0 → default [8, 16]
	extraTOML     string
}

func newEnv(t *testing.T, opt func(*envOptions)) *e2eEnv {
	t.Helper()
	o := &envOptions{warmupSilence: 10000, warmupLadder: []int{8, 16}}
	if opt != nil {
		opt(o)
	}

	hh := newFakeHH(t)
	ai := newFakeAI(t)
	dir := t.TempDir()

	var cfg strings.Builder
	fmt.Fprintf(&cfg, `
[general]
data_dir = "%s"
timezone = "UTC"
dry_run = %v

[storage]
path = "%s"

[log]
level = "error"
format = "text"
redact_secrets = true

[api.endpoints]
site = "%s"
chatik = "%s"
search_page = "%s/search/vacancy"

[accounts.main]
display_name = "E2E Account"
user_agent = "hhauto-e2e-agent/1.0"
rps = 50
concurrency = 1
daily_apply_cap = 200

[features.multiapply]
enabled = true
ai_filter = false
min_match_percent = 60

[features.chatops]
enabled = %v
poll_interval = "5m"

[warmup]
enabled = true
silence_hours = %d
ladder_hours = [%s]
max_pings = 3
source = "list"
ping_list = ["%s"]
respect_weekend = false

[runs.e2e]
enabled = true
account = "main"
resume_id = "%s"
query = ""
max_per_day = 5

[runs.e2e.filters]
exclude_words = ["продажам"]
min_employer_rating = 4.0

[pacing]
mode = "burst"
night_pause = false

[schedule]
jitter_minutes = 0
missed_run_policy = "run_once"
max_catchup_hours = 6

[ai.providers.fake]
dialect = "openai-chat"
base_url = "%s"
api_key_env = "HHAUTO_E2E_AI_KEY"
timeout_seconds = 10

[ai.models.fast]
provider = "fake"
model = "%s"

[ai.tasks.cover_letter]
model_profile = "fast"

[ai.tasks.message_classification]
model_profile = "fast"

[ai.tasks.chat_reply]
model_profile = "fast"

[ai.tasks.test_answer]
model_profile = "fast"
`,
		filepath.ToSlash(dir),
		o.dryRun,
		filepath.ToSlash(filepath.Join(dir, "hhauto.db")),
		hh.URL(), hh.URL(), hh.URL(),
		!o.chatsOff,
		o.warmupSilence, intsCSV(o.warmupLadder), e2ePing,
		resumeID,
		ai.URL(),
		e2eModel,
	)
	cfg.WriteString(o.extraTOML)

	cfgPath := filepath.Join(dir, "hhauto.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsfix.Seed(t, dir, "e2e-ua/1.0")

	// Deployment seams (same contract the CLI smoke tests use): the
	// encrypted-file cookie backend and the AI provider key.
	t.Setenv("HHAUTO_COOKIES_BACKEND", "encrypted-file")
	t.Setenv("HHAUTO_VAULT_PASS", "e2e-vault-passphrase")
	t.Setenv("HHAUTO_E2E_AI_KEY", "e2e-ai-key")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(dir, "hhauto.db"), store.Options{})
	if err != nil {
		t.Fatalf("harness store open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The scenario models an already-onboarded deployment: the accounts row
	// exists (created by onboarding / account import in production). The
	// resume write-through carries a FOREIGN KEY to accounts(id).
	if err := db.AccountsUpsert(ctx, store.Account{ID: acctID, DisplayName: "E2E Account"}); err != nil {
		t.Fatalf("seed account row: %v", err)
	}

	return &e2eEnv{
		t:       t,
		hh:      hh,
		ai:      ai,
		clock:   &testClock{},
		cfgPath: cfgPath,
		dataDir: dir,
		db:      db,
	}
}

func intsCSV(xs []int) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, fmt.Sprint(x))
	}
	return strings.Join(parts, ", ")
}

// runEngine builds a fresh Runtime through cli.Build (the full production
// wiring) and starts engine.Run with the injected clock. The caller polls
// for the scenario's end-state, then calls stopEngine.
func (env *e2eEnv) runEngine() (context.CancelFunc, <-chan error) {
	env.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rt, err := cli.Build(ctx, cli.BuildOptions{ConfigPath: env.cfgPath})
	if err != nil {
		cancel()
		env.t.Fatalf("cli.Build: %v", err)
	}
	apis, err := rt.AllAPIs()
	if err != nil {
		_ = rt.Close() // teardown on the failure path
		cancel()
		env.t.Fatalf("rt.AllAPIs: %v", err)
	}
	deps := engine.Deps{
		Cfg:         rt.Cfg,
		DB:          rt.DB,
		Queue:       rt.Queue,
		Feats:       rt.Feats,
		Webhooks:    rt.Webhooks,
		Log:         rt.Log,
		CookieStore: rt.Cookies,
		ProxyReg:    rt.ProxyReg,
		Captcha:     rt.Captcha,
		AIRunner:    rt.AI,
		APIs:        apis,
		KillSwitch:  rt.Kill,
		Pacer:       rt.Pacer,
		Outbox:      rt.Outbox,
		Clock:       env.clock,
		Rnd:         rand.New(rand.NewSource(20260924)),
	}
	rt.MarkEngineOwned() // engine.Run performs the graceful shutdown
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, deps) }()
	return cancel, done
}

// stopEngine cancels the context and waits for the graceful shutdown.
func (env *e2eEnv) stopEngine(cancel context.CancelFunc, done <-chan error) {
	env.t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			env.t.Fatalf("engine.Run: %v", err)
		}
	case <-time.After(45 * time.Second):
		env.t.Fatal("engine.Run did not shut down within 45s")
	}
}

// waitUntil polls cond every 20ms until it holds or the deadline expires
// (the sanctioned event-sync: no fixed sleeps — tests end exactly when the
// expected state appears).
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("e2e: condition not met within %s: %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// metricValue returns the counter for key summed over today and yesterday
// (a scenario that crosses a UTC midnight still observes its own counters).
func (env *e2eEnv) metricValue(key string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sum := 0
	for _, day := range []string{audit.Day(), audit.DayAt(time.Now().AddDate(0, 0, -1))} {
		m, err := env.db.MetricsSnapshot(ctx, day)
		if err != nil {
			env.t.Logf("metrics snapshot %s: %v", day, err)
			continue
		}
		sum += m[key]
	}
	return sum
}

// intentStep probes an idempotency intent: IntentReconcileFirst means the
// intent is DONE (the side effect settled), IntentSkipPending means another
// attempt is in flight, IntentProceed means the key was never claimed (the
// probe creates and immediately abandons the row to restore retryability).
func (env *e2eEnv) intentStep(key string) featurekit.IntentStep {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claim, err := featurekit.BeginIntent(ctx, env.db, key, "e2e-probe")
	if err != nil {
		env.t.Fatalf("intent probe %s: %v", key, err)
	}
	if claim.Step == featurekit.IntentProceed {
		_ = featurekit.AbandonIntent(ctx, env.db, claim)
	}
	return claim.Step
}

// chatMirrorState reads the chatops persisted chat-state mirror
// (feature_state namespace "chatops", key "state:<chatikID>").
func (env *e2eEnv) chatMirrorState(chatikID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, _, err := env.db.FeatureStateGet(ctx, "chatops", acctID, "state:"+chatikID)
	if err != nil {
		env.t.Fatalf("chat state probe %s: %v", chatikID, err)
	}
	return val
}

// featureState reads any feature_state value ("", false) when absent.
func (env *e2eEnv) featureState(feature, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	val, _, err := env.db.FeatureStateGet(ctx, feature, acctID, key)
	if err != nil {
		env.t.Fatalf("feature state probe %s/%s: %v", feature, key, err)
	}
	return val
}

// application returns the application row for a vacancy (nil when absent).
func (env *e2eEnv) application(vacancyID int64) *store.Application {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app, err := env.db.ApplicationsGetByDedupKey(ctx, featurekit.DedupKey(acctID, resumeID, vacancyID))
	if err != nil {
		env.t.Fatalf("application probe %d: %v", vacancyID, err)
	}
	return app
}
