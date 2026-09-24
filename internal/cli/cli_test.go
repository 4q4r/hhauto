package cli

// Command-level tests through the cobra tree (global flags, exit paths).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"hhauto/internal/store"
)

// runRoot executes the CLI with args against a fresh root command and
// returns the combined output.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func newContext() context.Context { return context.Background() }

func TestConfigValidateCmd(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	out, err := runRoot(t, "config", "validate", "--config", cfgPath)
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	mustContain(t, out, "OK")

	// An invalid config fails the command.
	bad := filepath.Join(filepath.Dir(cfgPath), "bad.toml")
	if werr := os.WriteFile(bad, []byte("[general]\ntimezone = \"Nope/Nope\"\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if _, err := runRoot(t, "config", "validate", "--config", bad); err == nil {
		t.Fatal("invalid config accepted")
	}
}

func TestConfigTemplateCmd(t *testing.T) {
	out, err := runRoot(t, "config", "template")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	for _, section := range []string{"[general]", "[storage]", "[log]", "[api.endpoints]", "[captcha.cap1]",
		"[proxy.socks5-1]", "[accounts.main]", "[ai.providers.fast]", "[ai.models.scorer]",
		"[features.auto_lift]", "[features.multiapply]", "[features.chatops]", "[pacing]",
		"[webhook.tg]", "[runs.backend-go]", "[review]", "[forms]", "[email]", "[warmup]", "[schedule]"} {
		if !strings.Contains(out, section) {
			t.Fatalf("template missing %s", section)
		}
	}
	// The template must stay valid TOML for the loader even though every
	// value is commented: check via a temp copy through config validate.
	dir := t.TempDir()
	path := filepath.Join(dir, "hhauto.toml")
	if werr := os.WriteFile(path, []byte(out), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if _, err := runRoot(t, "config", "validate", "--config", path); err != nil {
		t.Fatalf("template does not validate: %v\n%s", err, out)
	}
}

func TestReviewResolveAndIgnoreCmd(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)

	// Seed two open items.
	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	id1, aerr := rt.DB.ReviewItemsAdd(newContext(), "main", "session_expired", "seed one", `{"target":"a"}`)
	if aerr != nil {
		t.Fatal(aerr)
	}
	id2, aerr := rt.DB.ReviewItemsAdd(newContext(), "main", "test_unsure", "seed two", `{"target":"b"}`)
	if aerr != nil {
		t.Fatal(aerr)
	}
	_ = rt.Close() // test teardown

	out, err := runRoot(t, "review", "resolve", "not-a-number", "--config", cfgPath)
	if err == nil {
		t.Fatal("non-numeric id accepted")
	}
	_, _ = out, err

	if _, err := runRoot(t, "review", "resolve", itoa(id1), "--note", "refreshed cookies", "--config", cfgPath); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := runRoot(t, "review", "ignore", itoa(id2), "--config", cfgPath); err != nil {
		t.Fatalf("ignore: %v", err)
	}

	rt2, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt2.Close() }() // test teardown
	open, oerr := rt2.DB.ReviewItemsOpen(newContext(), "main", 100)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if len(open) != 0 {
		t.Fatalf("open review items remain: %+v", open)
	}
}

func TestReviewListCmd(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ReviewItemsAdd(newContext(), "main", "offer_received", "seed", `{}`); err != nil {
		t.Fatal(err)
	}
	_ = rt.Close() // test teardown

	out, err := runRoot(t, "review", "list", "--config", cfgPath)
	if err != nil {
		t.Fatalf("review list: %v", err)
	}
	mustContain(t, out, "offer_received")
}

func TestProfileEditCmd(t *testing.T) {
	cfgPath, dataDir := newFakeHHConfig(t)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true") // exits 0 without touching the file

	profilePath := filepath.Join(dataDir, "profile.md")
	if _, err := runRoot(t, "profile", "edit", "--config", cfgPath); err != nil {
		t.Fatalf("profile edit: %v", err)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("profile.md not created: %v", err)
	}
	if !strings.Contains(string(data), "Профиль кандидата") {
		t.Fatalf("profile.md template head missing: %s", data)
	}
}

func TestStatusCmdText(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	out, err := runRoot(t, "status", "--config", cfgPath)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	mustContain(t, out, "main", "unknown", "Funnel today")
}

func TestFunnelCmdText(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	out, err := runRoot(t, "funnel", "--days", "2", "--config", cfgPath)
	if err != nil {
		t.Fatalf("funnel: %v\n%s", err, out)
	}
	mustContain(t, out, "DAY", "SCANNED")
}

func TestAIEvalCmdGolden(t *testing.T) {
	golden := "../ai/testdata/golden"
	if _, err := os.Stat(golden); err != nil {
		t.Skipf("golden catalog not reachable from the package dir: %v", err)
	}
	t.Setenv("HHAUTO_GOLDEN_DIR", golden)
	out, err := runRoot(t, "ai", "eval")
	if err != nil {
		t.Fatalf("ai eval: %v\n%s", err, out)
	}
	mustContain(t, out, "провалено: 0")
}

func TestVersionCmd(t *testing.T) {
	out, err := runRoot(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "hhauto")
}

func TestAccountListCmd(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	out, err := runRoot(t, "account", "list", "--config", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "main", "Test")
}

func TestAccountRemoveRequiresYes(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	if _, err := runRoot(t, "account", "remove", "main", "--config", cfgPath); err == nil {
		t.Fatal("remove without --yes must fail")
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// stop arms the kill switch through the config's data_dir: the STOP file
// must exist at engine.StopFilePath and the path must be echoed back.
func TestStopCmdWritesKillSwitchFile(t *testing.T) {
	cfgPath, dataDir := newFakeHHConfig(t)

	out, err := runRoot(t, "stop", "--config", cfgPath)
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	stopPath := filepath.Join(dataDir, "STOP")
	if _, serr := os.Stat(stopPath); serr != nil {
		t.Fatalf("STOP file not written: %v", serr)
	}
	if !strings.Contains(out, stopPath) {
		t.Fatalf("output must include the STOP path %q, got: %s", stopPath, out)
	}
}

// ai compare aggregates ai_decisions per (prompt_ver, model) for the two
// experiment keys over the period and prints the comparison table.
func TestAICompareCmd(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)

	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx := newContext()
	must := func(d *store.AIDecision) {
		t.Helper()
		if aerr := rt.DB.AIDecisionsAdd(ctx, d); aerr != nil {
			t.Fatal(aerr)
		}
	}
	must(&store.AIDecision{AccountID: "main", Task: "vacancy_scoring", PromptVer: "v1", Model: "m1", NeedsHuman: true, Confidence: ptrF(0.6), LatencyMS: 400, CostUSD: 0.01})
	must(&store.AIDecision{AccountID: "main", Task: "vacancy_scoring", PromptVer: "v1", Model: "m1", NeedsHuman: false, Confidence: ptrF(0.8), LatencyMS: 600, CostUSD: 0.01})
	must(&store.AIDecision{AccountID: "main", Task: "vacancy_scoring", PromptVer: "v2", Model: "m1", NeedsHuman: false, LatencyMS: 100, CostUSD: 0.02})
	if cerr := rt.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	out, err := runRoot(t, "ai", "compare", "v1", "v2", "--config", cfgPath)
	if err != nil {
		t.Fatalf("ai compare: %v\n%s", err, out)
	}
	for _, want := range []string{"v1", "v2", "m1", "NEEDS_HUMAN", "CONFIDENCE", "LATENCY", "COST"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	jsonOut, err := runRoot(t, "ai", "compare", "v1", "v2", "--json", "--config", cfgPath)
	if err != nil {
		t.Fatalf("ai compare --json: %v\n%s", err, jsonOut)
	}
	for _, want := range []string{`"prompt_ver": "v1"`, `"prompt_ver": "v2"`, `"needs_human_pct": 50`} {
		if !strings.Contains(jsonOut, want) {
			t.Fatalf("json output missing %q:\n%s", want, jsonOut)
		}
	}
}

func ptrF(v float64) *float64 { return &v }
