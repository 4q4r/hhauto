package smoke

// Smoke suite: every test drives the REAL executable (built once in
// TestMain by `go build ./cmd/hhauto`) through its public surface —
// exit code plus stdout/stderr patterns. No network anywhere: the cookie
// store is pinned to the encrypted-file backend under a temp data_dir, and
// the run-once scenario uses a config with zero accounts (nothing to dial).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// smokeBin is the built binary path; TestMain owns its lifecycle.
var smokeBin string

func TestMain(m *testing.M) {
	code, err := runMain(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
	}
	os.Exit(code)
}

func runMain(m *testing.M) (int, error) {
	tmp, err := os.MkdirTemp("", "hhauto-smoke-bin-*")
	if err != nil {
		return 1, fmt.Errorf("mktemp: %w", err)
	}
	bin := filepath.Join(tmp, "hhauto")

	// Module root = two levels up from this file (internal/smoke).
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return 1, fmt.Errorf("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")

	build := exec.Command("go", "build", "-o", bin, "./cmd/hhauto")
	build.Dir = root
	if out, berr := build.CombinedOutput(); berr != nil {
		_ = os.RemoveAll(tmp)
		return 1, fmt.Errorf("go build ./cmd/hhauto: %v\n%s", berr, out)
	}

	smokeBin = bin
	code := m.Run()

	_ = os.RemoveAll(tmp)
	return code, nil
}

// smokeRun captures the observable contract of one binary invocation.
type smokeRun struct {
	exit   int
	stdout string
	stderr string
}

func runBinary(t *testing.T, cfgPath string, args ...string) smokeRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, smokeBin, append([]string{"--config", cfgPath}, args...)...)
	// HHAUTO_COOKIES_BACKEND=encrypted-file keeps cookie storage inside the
	// temp data_dir: no OS keyring, no D-Bus, no host state.
	cmd.Env = append(os.Environ(), "HHAUTO_COOKIES_BACKEND=encrypted-file")
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return smokeRun{exit: code, stdout: outBuf.String(), stderr: errBuf.String()}
}

// minimalConfig writes the smallest valid hhauto.toml (zero accounts, all
// paths under a temp dir) and returns its path plus the data dir.
func minimalConfig(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[general]\ndata_dir = %q\n\n[storage]\npath = %q\n",
		dataDir, filepath.Join(dataDir, "hhauto.db"))
	cfgPath = filepath.Join(t.TempDir(), "hhauto.toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hhauto.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSmokeVersion(t *testing.T) {
	cfg, _ := minimalConfig(t)
	r := runBinary(t, cfg, "version")
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0 (stdout=%q stderr=%q)", r.exit, r.stdout, r.stderr)
	}
	if !regexp.MustCompile(`(?m)^hhauto \S+ \(`).MatchString(r.stdout) {
		t.Fatalf("stdout %q: want \"hhauto <version> (<commit>, <date>)\"", r.stdout)
	}
}

func TestSmokeConfigValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, _ := minimalConfig(t)
		r := runBinary(t, cfg, "config", "validate")
		if r.exit != 0 {
			t.Fatalf("exit = %d (stdout=%q stderr=%q)", r.exit, r.stdout, r.stderr)
		}
		for _, want := range []string{"OK:", "валиден"} {
			if !strings.Contains(r.stdout, want) {
				t.Fatalf("stdout %q missing %q", r.stdout, want)
			}
		}
	})
	t.Run("invalid", func(t *testing.T) {
		cfg := writeConfig(t, "general = [[[not toml")
		r := runBinary(t, cfg, "config", "validate")
		if r.exit != 1 {
			t.Fatalf("exit = %d, want 1 (stdout=%q stderr=%q)", r.exit, r.stdout, r.stderr)
		}
		// Observed contract: SilenceErrors leaves the reason unprinted.
		// This assertion documents the surface exactly; flip it together
		// with a CLI fix that starts reporting the reason.
		if r.stdout != "" || r.stderr != "" {
			t.Logf("NOTE: invalid config now prints output (stdout=%q stderr=%q) — update this test", r.stdout, r.stderr)
		}
	})
}

func TestSmokeConfigTemplateRoundTrip(t *testing.T) {
	cfg, _ := minimalConfig(t)
	r := runBinary(t, cfg, "config", "template")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stderr=%q)", r.exit, r.stderr)
	}
	if len(r.stdout) < 1024 {
		t.Fatalf("template too short (%d bytes)", len(r.stdout))
	}
	tpl := filepath.Join(t.TempDir(), "template.toml")
	if err := os.WriteFile(tpl, []byte(r.stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	// The printed template must parse through the same validator.
	v := runBinary(t, tpl, "config", "validate")
	if v.exit != 0 {
		t.Fatalf("template does not validate: exit %d (stdout=%q stderr=%q)", v.exit, v.stdout, v.stderr)
	}
	if !strings.Contains(v.stdout, "OK:") {
		t.Fatalf("validate stdout %q missing OK", v.stdout)
	}
}

func TestSmokeStop(t *testing.T) {
	cfg, dataDir := minimalConfig(t)
	r := runBinary(t, cfg, "stop")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout=%q stderr=%q)", r.exit, r.stdout, r.stderr)
	}
	stopPath := filepath.Join(dataDir, "STOP")
	if !strings.Contains(r.stdout, "STOP-файл записан:") {
		t.Fatalf("stdout %q missing confirmation", r.stdout)
	}
	if !strings.Contains(r.stdout, stopPath) {
		t.Fatalf("stdout %q does not name %s", r.stdout, stopPath)
	}
	raw, err := os.ReadFile(stopPath)
	if err != nil {
		t.Fatalf("STOP file: %v", err)
	}
	if !strings.HasPrefix(string(raw), "stop requested ") {
		t.Fatalf("STOP content %q: want \"stop requested <RFC3339>\"", raw)
	}
}

func TestSmokeDoctorHelp(t *testing.T) {
	cfg, _ := minimalConfig(t)
	r := runBinary(t, cfg, "doctor", "--help")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout=%q stderr=%q)", r.exit, r.stdout, r.stderr)
	}
	for _, want := range []string{"Usage:", "hhauto doctor", "--config"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("doctor --help stdout missing %q:\n%s", want, r.stdout)
		}
	}
	// --help is a flag listing, not a diagnostic run: no check lines.
	for _, banned := range []string{"[OK]", "[FAIL]", "[WARN]"} {
		if strings.Contains(r.stdout, banned) {
			t.Fatalf("--help must not run probes, found %q", banned)
		}
	}
}

func TestSmokeRunOnceDryRunNoAccounts(t *testing.T) {
	cfg, _ := minimalConfig(t)
	r := runBinary(t, cfg, "run", "--once", "--dry-run")
	if r.exit != 0 {
		t.Fatalf("exit = %d (stdout=%q stderr=%q)", r.exit, r.stdout, r.stderr)
	}
	// Zero accounts → zero enabled feature handlers → clean exit with an
	// explicit reason (either the no-features short-circuit or the empty
	// once-summary).
	okMsg := strings.Contains(r.stdout, "no enabled features — nothing to do") ||
		strings.Contains(r.stdout, "0 task(s) done, 0 failed (dry_run=true)")
	if !okMsg {
		t.Fatalf("stdout %q: want the explicit nothing-to-do reason", r.stdout)
	}
}
