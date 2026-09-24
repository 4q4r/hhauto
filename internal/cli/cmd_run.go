package cli

// run / status / funnel — the lifecycle and observation commands.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"hhauto/internal/audit"
	"hhauto/internal/engine"
	"hhauto/internal/queue"
)

func newRunCmd() *cobra.Command {
	var (
		once    bool
		profile string
		feature string
		account string
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the engine until interrupted (--once: process due tasks now and exit)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := buildOpts()
			opts.Profile = profile
			opts.Feature = feature
			opts.Account = account
			if dryRun {
				opts.DryRun = true
			}
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			if once {
				_, _, err := runOnce(ctx, rt, outFor(cmd))
				return err
			}
			rt.MarkEngineOwned() // engine.Run performs the graceful shutdown
			apis, err := rt.AllAPIs()
			if err != nil {
				return exitErr("api clients: %v", err)
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
			}
			return engine.Run(ctx, deps)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "process the tasks that are due right now, then exit")
	cmd.Flags().StringVar(&profile, "profile", "", "restrict to one run profile id")
	cmd.Flags().StringVar(&feature, "feature", "", "restrict to one feature (autolift|multiapply|chatops|testsolve|contacts|warmup|emaildup)")
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account id")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "force dry-run for this invocation")
	return cmd
}

// stop arms the kill-switch file channel without touching the engine: a
// running `hhauto run` polls <data_dir>/STOP and shuts down gracefully. It
// reads only the config, so it works even when the engine is wedged.
func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Arm the kill switch: write <data_dir>/STOP so a running engine stops gracefully",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("%v", err)
			}
			stopPath := engine.StopFilePath(cfg.General.DataDir)
			note := "stop requested " + time.Now().UTC().Format(time.RFC3339) + "\n"
			if werr := os.WriteFile(stopPath, []byte(note), 0o640); werr != nil {
				return exitErr("write %s: %v", stopPath, werr)
			}
			outFor(cmd).printf("STOP-файл записан: %s\nРаботающий движок остановится при следующей проверке kill-switch (полл ≤1с, тик планировщика ≤30с).\n", stopPath)
			return nil
		},
	}
}

// runOnce processes everything currently queued and exits: features seed
// their due work at wiring time, consumers drain the queue through the
// registered handlers, and once the queue reaches quiescence (nothing
// queued, nothing in flight) intake closes and the runner exits. The
// runtime stays open: the caller owns teardown.
//
// Scope guard: --once processes each (account, kind, dedup key) at most
// once and stops after onceBudget — follow-up enqueues (e.g. multiapply's
// re-armed "tomorrow" task) are not chased into the future.
func runOnce(ctx context.Context, rt *Runtime, o out) (int, int, error) {
	handlers := rt.Handlers()
	if len(handlers) == 0 {
		o.println("run --once: no enabled features — nothing to do")
		return 0, 0, nil
	}

	accounts := make([]string, 0, len(rt.Cfg.Accounts))
	for id := range rt.Cfg.Accounts {
		accounts = append(accounts, id)
	}
	sort.Strings(accounts)

	// One best-effort session bootstrap per account (the engine does this
	// per task with a TTL; a single pass keeps --once cheap).
	for _, id := range accounts {
		api, err := rt.APIFor(id)
		if err != nil {
			rt.Log.Warn("run --once: api client unavailable", "account", id, "err", err)
			continue
		}
		if err := api.BootstrapSession(ctx); err != nil {
			rt.Log.Warn("run --once: session bootstrap failed; proceeding", "account", id, "err", err)
		}
	}

	var (
		wg        sync.WaitGroup
		inflight  atomic.Int64
		seen      sync.Map // (account|kind|dedupkey) → processed in this pass
		processed atomic.Int64
		failed    atomic.Int64
	)

	drain := func(accountID string, task queue.Task) {
		key := accountID + "|" + string(task.Kind) + "|" + task.DedupKey
		if _, dup := seen.LoadOrStore(key, true); dup {
			return // processed earlier in this pass: drop the re-enqueue
		}
		h, known := handlers[task.Kind]
		if !known {
			rt.Log.Error("run --once: no handler for task kind", "kind", task.Kind, "task", task.ID)
			_ = audit.Incr(ctx, rt.DB, audit.KeyTasksFailed)
			failed.Add(1)
			return
		}
		tc, err := rt.TaskContext(accountID)
		if err != nil {
			rt.Log.Error("run --once: task context", "account", accountID, "err", err)
			failed.Add(1)
			return
		}
		if err := h(ctx, tc, task); err != nil {
			rt.Log.Error("run --once: task failed", "account", accountID, "kind", task.Kind, "task", task.ID, "err", err)
			_ = audit.Incr(ctx, rt.DB, audit.KeyTasksFailed)
			failed.Add(1)
			return
		}
		rt.Log.Info("run --once: task done", "account", accountID, "kind", task.Kind, "task", task.ID)
		processed.Add(1)
	}

	// Consumers: one per account. Each exits when the queue closes (or the
	// context is cancelled); the in-flight counter covers the full
	// dequeue→handler span, so the supervisor below cannot mistake a task
	// being handled for a quiescent queue.
	for _, id := range accounts {
		wg.Add(1)
		go func(accountID string) {
			defer wg.Done()
			for {
				task, ok := rt.Queue.Dequeue(ctx, accountID)
				if !ok {
					return
				}
				if rt.Kill.Tripped(ctx) {
					return
				}
				// Counted only while a task is actually in hand: a consumer
				// parked in Dequeue must not keep the supervisor waiting.
				inflight.Add(1)
				drain(accountID, task)
				inflight.Add(-1)
			}
		}(id)
	}

	// Supervisor: watch for quiescence (nothing queued anywhere, nothing
	// in flight on two consecutive samples), then close intake so parked
	// consumers exit. The double sample closes the dequeue→count race: a
	// task that left the queue but is not yet counted makes the second
	// sample non-quiescent.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	budget := time.NewTimer(onceBudget)
	defer budget.Stop()
	quiet := 0
quiesce:
	for {
		select {
		case <-ctx.Done():
			break quiesce
		case <-budget.C:
			rt.Log.Warn("run --once: time budget reached; stopping the drain")
			break quiesce
		case <-ticker.C:
			total := 0
			for _, id := range accounts {
				total += rt.Queue.Len(id)
			}
			if total == 0 && inflight.Load() == 0 {
				quiet++
				if quiet >= 2 {
					break quiesce
				}
			} else {
				quiet = 0
			}
		}
	}
	_ = rt.Queue.Close(context.Background(), time.Second)
	wg.Wait()

	p, f := processed.Load(), failed.Load()
	o.printf("run --once: %d task(s) done, %d failed (dry_run=%v)\n", p, f, rt.Cfg.General.DryRun)
	if f > 0 {
		return int(p), int(f), exitErr("%d task(s) failed", f)
	}
	return int(p), int(f), nil
}

// onceBudget bounds a single --once pass.
const onceBudget = 5 * time.Minute

func newStatusCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Account states, today's counters, open reviews, next scheduled runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			return showStatus(ctx, rt, outFor(cmd), account)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account id")
	return cmd
}

// statusReport is the structured status document (--json).
type statusReport struct {
	Day        string           `json:"day"`
	Accounts   []statusAccount  `json:"accounts"`
	Funnel     audit.Funnel     `json:"funnel"`
	Counters   map[string]int   `json:"counters"`
	AITokens   int              `json:"ai_tokens_today"`
	AICostUSD  float64          `json:"ai_cost_usd_today"`
	ReviewOpen map[string]int   `json:"review_open_by_account"`
	Schedules  []statusSchedule `json:"schedules"`
}

type statusAccount struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Proxy  string `json:"proxy_ref,omitempty"`
}

type statusSchedule struct {
	Name    string `json:"name"`
	Account string `json:"account"`
	NextRun string `json:"next_run"`
}

func showStatus(ctx context.Context, rt *Runtime, o out, accountFilter string) error {
	day := audit.Day()
	rep := statusReport{Day: day, Counters: map[string]int{}}

	stored, err := rt.DB.AccountsList(ctx)
	if err != nil {
		return exitErr("accounts: %v", err)
	}
	statuses := make(map[string]string, len(stored))
	for _, a := range stored {
		statuses[a.ID] = a.Status
	}

	rep.ReviewOpen = map[string]int{}
	ids := make([]string, 0, len(rt.Cfg.Accounts))
	for id := range rt.Cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if accountFilter != "" && id != accountFilter {
			continue
		}
		a := statusAccount{ID: id, Status: statuses[id], Proxy: rt.Cfg.Accounts[id].ProxyRef}
		if a.Status == "" {
			a.Status = "unknown"
		}
		rep.Accounts = append(rep.Accounts, a)
		open, err := rt.DB.ReviewItemsOpen(ctx, id, 500)
		if err != nil {
			return exitErr("review open %s: %v", id, err)
		}
		rep.ReviewOpen[id] = len(open)
	}

	funnel, err := audit.Snapshot(ctx, rt.DB, day)
	if err != nil {
		return exitErr("funnel: %v", err)
	}
	rep.Funnel = funnel
	metrics, err := rt.DB.MetricsSnapshot(ctx, day)
	if err != nil {
		return exitErr("metrics: %v", err)
	}
	for _, k := range []string{audit.KeyHTTP429, audit.KeyCaptchasSolved, audit.KeyTasksFailed, audit.KeyPingsSent, audit.KeyEmailsSent} {
		if v := metrics[k]; v != 0 {
			rep.Counters[k] = v
		}
	}
	rep.AITokens, rep.AICostUSD, err = rt.DB.AIDecisionsSumTokensDayAll(ctx, day)
	if err != nil {
		return exitErr("ai totals: %v", err)
	}

	// Next runs come from the registered bindings (specs live in the
	// schedule registry, not in a readable store DAO).
	now := time.Now()
	rnd := rand.New(rand.NewSource(1)) // deterministic preview; real jitter happens per fire
	for _, b := range rt.Schedules() {
		for _, id := range ids {
			if accountFilter != "" && id != accountFilter {
				continue
			}
			spec := b.Spec
			if spec.TZ == "" {
				spec.TZ = rt.Cfg.General.Timezone
			}
			next := spec.Next(now, rnd)
			rep.Schedules = append(rep.Schedules, statusSchedule{
				Name: b.Name, Account: id, NextRun: next.Format(time.RFC3339),
			})
		}
	}

	if flags.json {
		return o.printJSON(rep)
	}

	o.println("Day:", day)
	o.printTable([]string{"ACCOUNT", "STATUS", "PROXY", "REVIEW OPEN"},
		func() [][]string {
			rows := make([][]string, 0, len(rep.Accounts))
			for _, a := range rep.Accounts {
				rows = append(rows, []string{a.ID, a.Status, a.Proxy, fmt.Sprint(rep.ReviewOpen[a.ID])})
			}
			return rows
		}())
	o.printf("\nFunnel today: scanned=%d passed=%d applied=%d skipped=%d stage-skips:",
		funnel.Scanned, funnel.Passed, funnel.Applied, totalSkipped(funnel))
	if len(funnel.SkippedByStage) == 0 {
		o.println(" -")
	} else {
		stages := make([]string, 0, len(funnel.SkippedByStage))
		for stage, n := range funnel.SkippedByStage {
			stages = append(stages, fmt.Sprintf("%s=%d", stage, n))
		}
		sort.Strings(stages)
		o.println(" " + joinComma(stages))
	}
	o.printf("AI today: tokens=%d cost=$%.4f\n", rep.AITokens, rep.AICostUSD)
	if len(rep.Schedules) > 0 {
		rows := make([][]string, 0, len(rep.Schedules))
		for _, s := range rep.Schedules {
			rows = append(rows, []string{s.Name, s.Account, s.NextRun})
		}
		o.println("\nNext runs:")
		o.printTable([]string{"SCHEDULE", "ACCOUNT", "NEXT RUN"}, rows)
	}
	return nil
}

func totalSkipped(f audit.Funnel) int {
	n := 0
	for _, v := range f.SkippedByStage {
		n += v
	}
	return n
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func newFunnelCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "funnel",
		Short: "Daily funnel: scanned → passed → applied → stage skips",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days <= 0 {
				days = 7
			}
			ctx := cmd.Context()
			rt, err := Build(ctx, buildOpts())
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			return showFunnel(ctx, rt, outFor(cmd), days)
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "how many days to show (including today)")
	return cmd
}

// funnelRow is one day of the funnel report (--json too).
type funnelRow struct {
	Day    string       `json:"day"`
	Funnel audit.Funnel `json:"funnel"`
}

// showFunnel renders the per-day funnel from the metrics counters.
func showFunnel(ctx context.Context, rt *Runtime, o out, days int) error {
	rows := make([]funnelRow, 0, days)
	today := time.Now().UTC()
	for i := days - 1; i >= 0; i-- {
		day := audit.DayAt(today.AddDate(0, 0, -i))
		f, err := audit.Snapshot(ctx, rt.DB, day)
		if err != nil {
			return exitErr("funnel %s: %v", day, err)
		}
		rows = append(rows, funnelRow{Day: day, Funnel: f})
	}
	if flags.json {
		return o.printJSON(rows)
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.Day,
			fmt.Sprint(r.Funnel.Scanned),
			fmt.Sprint(r.Funnel.Passed),
			fmt.Sprint(r.Funnel.Applied),
			fmt.Sprint(totalSkipped(r.Funnel)),
		})
	}
	o.printTable([]string{"DAY", "SCANNED", "PASSED", "APPLIED", "SKIPPED"}, table)
	return nil
}
