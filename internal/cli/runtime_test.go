package cli

// Integration smoke: the full wiring (config → store → cookies → proxy →
// captcha-absent → ai-absent → webhook → features → engine registration)
// against the fake hh, then one --once pass in dry-run.

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"hhauto/internal/audit"
	"hhauto/internal/queue"
)

func TestBuildWiringAndRunOnceDryRun(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	rt, err := Build(context.Background(), BuildOptions{ConfigPath: cfgPath, DryRun: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	// Features registered; the three enabled ones present.
	names := map[string]bool{}
	for _, f := range rt.Feats.All() {
		names[f.Name()] = true
	}
	for _, want := range []string{"autolift", "multiapply", "chatops", "testsolve", "contacts", "warmup", "emaildup"} {
		if !names[want] {
			t.Fatalf("feature %s missing from registry: %v", want, names)
		}
	}
	if got := len(rt.Feats.Enabled()); got != 3 {
		t.Fatalf("enabled features = %d, want 3", got)
	}

	// Handlers registered for every enabled feature kind.
	for _, kind := range []queue.TaskKind{queue.KindLift, queue.KindApply, queue.KindChatSync} {
		if _, ok := rt.Handlers()[kind]; !ok {
			t.Fatalf("handler for %s not registered", kind)
		}
	}

	// Engine schedule bindings exist for the feature schedules.
	bound := map[string]bool{}
	for _, b := range rt.Schedules() {
		bound[b.Name] = true
	}
	for _, want := range []string{"features.autolift", "chatops.poll", "runs.testrun"} {
		if !bound[want] {
			t.Fatalf("schedule %s not bound (bound: %v)", want, bound)
		}
	}

	// Seeds enqueued at wiring time.
	if rt.Queue.Len("main") == 0 {
		t.Fatal("queue empty after wiring: features did not seed tasks")
	}

	processed, failed, err := runOnce(context.Background(), rt, out{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed tasks = %d, want 0", failed)
	}
	if processed < 3 {
		t.Fatalf("processed = %d, want >= 3 (lift + apply + chat sync)", processed)
	}
	if rt.Queue.Len("main") != 0 {
		t.Fatalf("queue not drained: %d task(s) left", rt.Queue.Len("main"))
	}

	// 0 mutations: no applications, no successful lifts, no sent messages.
	day := audit.Day()
	n, err := rt.DB.ApplicationsCountToday(context.Background(), "main", "testrun", day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("applications created in dry-run: %d", n)
	}
	metrics, err := rt.DB.MetricsSnapshot(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"lift.ok", "chatops.reply_sent", "chatops.test_submitted", "apply.ok"} {
		if metrics[banned] != 0 {
			t.Fatalf("mutation metric %s = %d in dry-run", banned, metrics[banned])
		}
	}
	if metrics["lift.dry_run"] == 0 {
		t.Fatalf("autolift did not reach the dry-run branch: %v", metrics)
	}
}

func TestRunOnceRespectsDryRunFlagFromCommand(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	rt, err := Build(context.Background(), BuildOptions{ConfigPath: cfgPath, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Cfg.General.DryRun {
		t.Fatal("--dry-run did not propagate into the config copy before feature construction")
	}
	rt.Close()
}

func TestStatusJSON(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	rt, err := Build(context.Background(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	old := flags.json
	flags.json = true
	defer func() { flags.json = old }()

	var buf bytes.Buffer
	if err := showStatus(context.Background(), rt, out{stdout: &buf}, ""); err != nil {
		t.Fatal(err)
	}
	var rep statusReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("status --json: %v\n%s", err, buf.String())
	}
	if rep.Day == "" || len(rep.Accounts) != 1 || rep.Accounts[0].ID != "main" {
		t.Fatalf("unexpected status document: %s", buf.String())
	}
	if _, ok := rep.ReviewOpen["main"]; !ok {
		t.Fatalf("review_open_by_account missing main: %s", buf.String())
	}
}

func TestFunnelReport(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	rt, err := Build(context.Background(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Seed one day of funnel counters.
	day := audit.Day()
	if err := audit.IncrDay(context.Background(), rt.DB, audit.KeyVacanciesScanned, day); err != nil {
		t.Fatal(err)
	}
	if err := audit.IncrDay(context.Background(), rt.DB, audit.KeyApplicationsSent, day); err != nil {
		t.Fatal(err)
	}

	old := flags.json
	flags.json = true
	defer func() { flags.json = old }()

	var buf bytes.Buffer
	if err := showFunnel(context.Background(), rt, out{stdout: &buf}, 3); err != nil {
		t.Fatal(err)
	}
	var rows []funnelRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("funnel --json: %v\n%s", err, buf.String())
	}
	if len(rows) != 3 {
		t.Fatalf("funnel rows = %d, want 3", len(rows))
	}
	today := rows[len(rows)-1]
	if today.Day != day || today.Funnel.Scanned != 1 || today.Funnel.Applied != 1 {
		t.Fatalf("today's funnel row wrong: %+v (want day %s)", today, day)
	}
}

func TestStatusNextRunPreview(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	rt, err := Build(context.Background(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(rt.Schedules()) == 0 {
		t.Fatal("no schedules registered for next-run preview")
	}
	now := time.Now()
	rnd := rand.New(rand.NewSource(1))
	for _, b := range rt.Schedules() {
		if !b.Spec.Next(now, rnd).After(now) {
			t.Fatalf("schedule %s: next run not in the future", b.Name)
		}
	}
}
