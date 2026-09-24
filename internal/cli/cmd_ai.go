package cli

// ai status/eval, config validate/template, doctor, backup, version.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hhauto/internal/ai"
	"hhauto/internal/audit"
	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/engine"
	"hhauto/internal/hhapi"
	"hhauto/internal/proxy"
)

func newAICmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ai", Short: "AI subsystem (status | eval)"}
	cmd.AddCommand(newAIStatusCmd(), newAIEvalCmd())
	return cmd
}

func newAIStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "AI usage today: tokens and USD (from the decision ledger)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rt, err := Build(ctx, buildOpts())
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer rt.Close()
			o := outFor(cmd)
			day := audit.Day()
			tokens, usd, err := rt.DB.AIDecisionsSumTokensDayAll(ctx, day)
			if err != nil {
				return exitErr("%v", err)
			}
			type aiStatus struct {
				Day          string              `json:"day"`
				Enabled      bool                `json:"ai_enabled"`
				Tokens       int                 `json:"tokens_today"`
				CostUSD      float64             `json:"cost_usd_today"`
				BudgetTokens int                 `json:"budget_tokens"`
				BudgetUSD    float64             `json:"budget_usd"`
				PerAccount   map[string]aiAccUsg `json:"per_account,omitempty"`
			}
			rep := aiStatus{
				Day: day, Enabled: rt.AI != nil, Tokens: tokens, CostUSD: usd,
				BudgetTokens: rt.Cfg.AI.DailyBudgetTokens, BudgetUSD: rt.Cfg.AI.DailyBudgetUSD,
			}
			if len(rt.Cfg.Accounts) > 0 {
				rep.PerAccount = map[string]aiAccUsg{}
				for id := range rt.Cfg.Accounts {
					in, out, err := rt.DB.AIDecisionsSumTokensDay(ctx, id, day)
					if err != nil {
						return exitErr("%v", err)
					}
					rep.PerAccount[id] = aiAccUsg{TokensIn: in, TokensOut: out}
				}
			}
			if flags.json {
				return o.printJSON(rep)
			}
			o.printf("День %s: токены=%d, стоимость=$%.4f (ИИ %s)\n",
				day, rep.Tokens, rep.CostUSD, map[bool]string{true: "включён", false: "выключен"}[rep.Enabled])
			if rep.BudgetTokens > 0 {
				o.printf("Бюджет дня: %d токенов (использовано %.1f%%)\n", rep.BudgetTokens, 100*float64(rep.Tokens)/float64(rep.BudgetTokens))
			}
			if rep.BudgetUSD > 0 {
				o.printf("Бюджет дня: $%.2f (использовано %.1f%%)\n", rep.BudgetUSD, 100*rep.CostUSD/rep.BudgetUSD)
			}
			return nil
		},
	}
}

type aiAccUsg struct {
	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
}

func newAIEvalCmd() *cobra.Command {
	var task string
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run the golden-fixture regression catalog (exit 1 on any failure)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dir := goldenDir()
			results, err := ai.RunGolden(ctx, dir)
			if err != nil {
				return exitErr("%v (каталог: %s)", err, dir)
			}
			o := outFor(cmd)
			failed := 0
			type evalRow struct {
				Name string `json:"name"`
				Pass bool   `json:"pass"`
				Want string `json:"want,omitempty"`
				Got  string `json:"got,omitempty"`
				Err  string `json:"err,omitempty"`
			}
			rows := make([]evalRow, 0, len(results))
			for _, r := range results {
				pass := r.Err == nil
				if !pass {
					// needs_human invariant: an expected needs_human=true
					// verdict must never come back needs_human=false —
					// covered by verdict equality, surfaced explicitly here.
					failed++
				}
				rows = append(rows, evalRow{Name: r.Name, Pass: pass, Err: errString(r.Err)})
			}
			if task != "" {
				filtered := rows[:0:0]
				for _, r := range rows {
					if strings.Contains(r.Name, task) {
						filtered = append(filtered, r)
					}
				}
				rows = filtered
			}
			if flags.json {
				_ = o.printJSON(rows)
			} else {
				table := make([][]string, 0, len(rows))
				for _, r := range rows {
					mark := "ok"
					if !r.Pass {
						mark = "FAIL"
					}
					line := []string{r.Name, mark}
					if r.Err != "" {
						line = append(line, clip(r.Err, 80))
					}
					table = append(table, line)
				}
				o.printTable([]string{"CASE", "RESULT", "DETAIL"}, table)
			}
			o.printf("\nGolden: %d кейс(ов), провалено: %d (каталог: %s)\n", len(rows), failed, dir)
			if failed > 0 {
				return exitErr("%d golden-кейс(ов) провалено", failed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "filter cases by name substring (e.g. cover_letter)")
	return cmd
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// goldenDir resolves the fixture catalog: $HHAUTO_GOLDEN_DIR, ./golden, or
// the repo's embedded catalog when run from a source checkout.
func goldenDir() string {
	if v := os.Getenv("HHAUTO_GOLDEN_DIR"); v != "" {
		return v
	}
	for _, cand := range []string{"golden", "internal/ai/testdata/golden"} {
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand
		}
	}
	return "golden"
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Config operations (validate | template)"}
	cmd.AddCommand(newConfigValidateCmd(), newConfigTemplateCmd())
	return cmd
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Load and validate hhauto.toml (fail-fast with the offending path)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return exitErr("config invalid: %v", err)
			}
			applyCLIDefaults(cfg)
			o := outFor(cmd)
			o.printf("OK: %s валиден (аккаунтов: %d, фич включено: %d, провайдеров ИИ: %d)\n",
				flags.configPath, len(cfg.Accounts),
				countEnabledFeatures(cfg), len(cfg.AI.Tasks))
			return nil
		},
	}
}

func countEnabledFeatures(cfg *config.Config) int {
	n := 0
	for _, get := range featureFilters {
		if *get(cfg) {
			n++
		}
	}
	return n
}

func newConfigTemplateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "template",
		Short: "Print the full commented config template (docs/PLAN.md §5)",
		RunE: func(cmd *cobra.Command, args []string) error {
			o := outFor(cmd)
			o.printf("%s", configTemplate)
			return nil
		},
	}
}

func newBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Backup the SQLite database (VACUUM INTO), keep 7 copies",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("%v", err)
			}
			destDir := filepath.Join(cfg.General.DataDir, "backups")
			if err := os.MkdirAll(destDir, 0o750); err != nil {
				return exitErr("backup dir: %v", err)
			}
			name := "hhauto-" + time.Now().UTC().Format("20060102-150405") + ".db"
			dest := filepath.Join(destDir, name)

			// A dedicated read connection runs VACUUM INTO: the store
			// package exposes no raw SQL seam by contract. WAL allows the
			// read snapshot alongside a running engine.
			dsn := "file:" + cfg.Storage.Path + "?_pragma=busy_timeout(" + strconv.Itoa(cfg.Storage.BusyTimeoutMS) + ")"
			conn, err := sql.Open("sqlite", dsn)
			if err != nil {
				return exitErr("open: %v", err)
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			// The destination path is a quoted SQL literal: escape single quotes.
			safeDest := strings.ReplaceAll(dest, "'", "''")
			if _, err := conn.ExecContext(ctx, "VACUUM INTO '"+safeDest+"'"); err != nil {
				return exitErr("vacuum into: %v", err)
			}
			_ = conn.Close()

			kept, err := rotateBackups(destDir, 7)
			if err != nil {
				outFor(cmd).errf("ротация не удалась: %v", err)
			}
			outFor(cmd).printf("Бэкап: %s (осталось копий: %d)\n", dest, kept)
			return nil
		},
	}
}

// rotateBackups deletes the oldest backups beyond keep and returns how many
// remain.
func rotateBackups(dir string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "hhauto-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	// Names are timestamp-ordered, so lexicographic sort = chronological.
	sort.Strings(names)
	remaining := len(names)
	for remaining > keep {
		victim := names[0]
		names = names[1:]
		if err := os.Remove(filepath.Join(dir, victim)); err != nil {
			return remaining, err
		}
		remaining--
	}
	return remaining, nil
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, args []string) error {
			outFor(cmd).printf("hhauto %s (%s, %s)\n", buildVersion, buildCommit, buildDate)
			return nil
		},
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Environment diagnostics: config, disk, cookies, proxies, sessions, captcha, Chrome, AI",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return runDoctor(ctx, outFor(cmd))
		},
	}
}

// doctorCheck is one diagnostic line.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // OK | WARN | FAIL
	Detail string `json:"detail"`
}

func runDoctor(ctx context.Context, o out) error {
	cfg, err := LoadConfigOnly(flags.configPath)
	var checks []doctorCheck
	add := func(name, status, detail string) {
		checks = append(checks, doctorCheck{Name: name, Status: status, Detail: detail})
	}
	if err != nil {
		add("config", "FAIL", err.Error())
		return reportDoctor(o, checks)
	}
	add("version", "OK", buildVersion+" ("+buildCommit+")")
	add("config", "OK", flags.configPath)

	// Disk/paths.
	dataDir := cfg.General.DataDir
	if err := probeWritable(dataDir); err != nil {
		add("data_dir", "FAIL", dataDir+": "+err.Error())
	} else {
		add("data_dir", "OK", dataDir)
	}
	if err := probeWritable(filepath.Dir(cfg.Storage.Path)); err != nil {
		add("storage", "FAIL", cfg.Storage.Path+": "+err.Error())
	} else {
		add("storage", "OK", cfg.Storage.Path)
	}

	// Cookie store backend.
	backend := "-"
	if ckStore, cerr := openCookieStore(cfg); cerr != nil {
		add("cookies", "FAIL", cerr.Error())
	} else {
		backend = ckStore.Backend()
		add("cookies", "OK", "backend: "+backend)
	}

	// Direct egress IP first (baseline for the proxy comparison).
	directIP, directErr := egressIP(ctx, nil)
	if directErr != nil {
		add("network", "WARN", "прямой egress недоступен: "+directErr.Error())
	} else {
		add("network", "OK", "прямой egress IP: "+directIP)
	}
	for _, ref := range sortedProxyRefs(cfg) {
		p := cfg.Proxy[ref]
		dialer, derr := buildSingleDialer(cfg, ref)
		if derr != nil {
			add("proxy:"+ref, "FAIL", derr.Error())
			continue
		}
		ip, perr := egressIP(ctx, dialer)
		switch {
		case perr != nil:
			add("proxy:"+ref, "FAIL", fmt.Sprintf("%s:%d: %v", p.Host, p.Port, perr))
		case directErr == nil && ip == directIP:
			add("proxy:"+ref, "WARN", fmt.Sprintf("%s:%d: egress %s совпадает с прямым — прокси не работает?", p.Host, p.Port, ip))
		default:
			add("proxy:"+ref, "OK", fmt.Sprintf("%s:%d → egress %s", p.Host, p.Port, ip))
		}
	}

	// Sessions.
	if len(cfg.Accounts) == 0 {
		add("sessions", "WARN", "аккаунтов нет")
	}
	for _, id := range sortedAccountIDs(cfg) {
		if err := probeSession(ctx, cfg, id); err != nil {
			add("session:"+id, "FAIL", err.Error())
		} else {
			add("session:"+id, "OK", "bootstrap+me")
		}
	}

	// Captcha balances.
	if len(cfg.Captcha) == 0 {
		add("captcha", "OK", "провайдеры не настроены (капча-ветки выключены)")
	}
	for _, id := range sortedCaptchaIDs(cfg) {
		bal, err := captchaBalance(ctx, cfg.Captcha[id])
		if err != nil {
			add("captcha:"+id, "FAIL", err.Error())
		} else {
			add("captcha:"+id, "OK", fmt.Sprintf("баланс: %.2f", bal))
		}
	}

	// Chrome for headless forms.
	chrome, cerr := findChrome()
	if cfg.Forms.Mode == "headless" && cerr != nil {
		add("chrome", "FAIL", "forms.mode=headless, но Chrome не найден в PATH")
	} else if cerr != nil {
		add("chrome", "WARN", "не найден в PATH (нужен только для headless-форм)")
	} else {
		add("chrome", "OK", chrome)
	}

	// AI providers.
	if len(cfg.AI.Tasks) == 0 {
		add("ai", "OK", "задачи ИИ не настроены")
	}
	for _, id := range sortedAIProviderIDs(cfg) {
		p := cfg.AI.Providers[id]
		status, detail := probeAIProvider(ctx, p)
		add("ai:"+id, status, detail)
	}

	return reportDoctor(o, checks)
}

func reportDoctor(o out, checks []doctorCheck) error {
	if flags.json {
		if err := o.printJSON(checks); err != nil {
			return err
		}
	} else {
		rows := make([][]string, 0, len(checks))
		for _, c := range checks {
			rows = append(rows, []string{statusMark(c.Status), c.Name, c.Detail})
		}
		o.printTable(nil, rows)
	}
	failed := 0
	for _, c := range checks {
		if c.Status == "FAIL" {
			failed++
		}
	}
	if failed > 0 {
		return exitErr("doctor: %d проверок провалено", failed)
	}
	return nil
}

func probeWritable(dir string) error {
	if dir == "" {
		return fmt.Errorf("путь пуст")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// resolveCookieStore builds the cookie store with the deployment backend
// resolution (HHAUTO_COOKIES_BACKEND override, auto otherwise) and the
// data-dir blob path. The single seam for every command.
func resolveCookieStore(cfg *config.Config) (cookies.Store, error) {
	return cookies.New(cookies.StorageConfig{
		Backend: os.Getenv("HHAUTO_COOKIES_BACKEND"),
		Path:    filepath.Join(cfg.General.DataDir, "cookies"),
	})
}

func openCookieStore(cfg *config.Config) (cookies.Store, error) {
	return resolveCookieStore(cfg)
}

func sortedProxyRefs(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Proxy))
	for ref := range cfg.Proxy {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func sortedAccountIDs(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Accounts))
	for id := range cfg.Accounts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedCaptchaIDs(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Captcha))
	for id := range cfg.Captcha {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedAIProviderIDs(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.AI.Providers))
	for id := range cfg.AI.Providers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func buildSingleDialer(cfg *config.Config, ref string) (proxy.Dialer, error) {
	reg, err := proxy.NewRegistry(cfg.Proxy)
	if err != nil {
		return nil, err
	}
	return reg.Dialer(ref)
}

// buildSingleClient builds one live hh client for a diagnostic session
// probe. A nil cookie store is tolerated (fresh-jar bootstrap).
func buildSingleClient(ctx context.Context, cfg *config.Config, id string) (hhapi.HHAPI, error) {
	ckStore, err := openCookieStore(cfg)
	if err != nil {
		return nil, fmt.Errorf("cookie store: %w", err)
	}
	reg, err := proxy.NewRegistry(cfg.Proxy)
	if err != nil {
		return nil, err
	}
	apis, err := engine.BuildAPIClients(ctx, cfg, reg, ckStore, nil)
	if err != nil {
		return nil, err
	}
	api, ok := apis[id]
	if !ok {
		return nil, fmt.Errorf("нет клиента для аккаунта %q", id)
	}
	return api, nil
}

func egressIP(ctx context.Context, dialer proxy.Dialer) (string, error) {
	tr := &http.Transport{}
	if dialer != nil {
		tr.DialContext = dialer.DialContext
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	var doc struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.IP == "" {
		return "", fmt.Errorf("не удалось разобрать ответ")
	}
	return doc.IP, nil
}

func probeSession(ctx context.Context, cfg *config.Config, id string) error {
	api, err := buildSingleClient(ctx, cfg, id)
	if err != nil {
		return err
	}
	if err := api.BootstrapSession(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if _, err := api.Me(ctx); err != nil {
		return fmt.Errorf("me: %w", err)
	}
	return nil
}

func findChrome() (string, error) {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("chrome не найден")
}

// captchaBalance probes the provider's balance endpoint with the protocol
// each adapter speaks (2captcha: res.php getbalance; anticaptcha:
// getBalance). internal/captcha keeps its solvers private, so the doctor
// speaks the wire protocol directly — diagnostic only.
func captchaBalance(ctx context.Context, p config.CaptchaProvider) (float64, error) {
	key := os.Getenv(p.KeyEnv)
	if key == "" {
		key = p.Key
	}
	if key == "" {
		return 0, fmt.Errorf("ключ недоступен (key_env %s не задан)", p.KeyEnv)
	}
	base := strings.TrimRight(p.BaseURL, "/")
	client := &http.Client{Timeout: 10 * time.Second}
	switch p.Protocol {
	case "2captcha":
		url := base + "/res.php?key=" + key + "&action=getbalance&json=1"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		var doc struct {
			Status  int    `json:"status"`
			Request string `json:"request"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return 0, fmt.Errorf("ответ: %s", clip(string(body), 80))
		}
		if doc.Status != 1 {
			return 0, fmt.Errorf("провайдер: %s", doc.Request)
		}
		return strconv.ParseFloat(doc.Request, 64)
	case "anticaptcha":
		payload, _ := json.Marshal(map[string]any{"clientKey": key, "task": "getBalance"})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", strings.NewReader(string(payload)))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		var doc struct {
			ErrorID int     `json:"errorId"`
			Balance float64 `json:"balance"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return 0, fmt.Errorf("ответ: %s", clip(string(body), 80))
		}
		if doc.ErrorID != 0 {
			return 0, fmt.Errorf("провайдер: errorId=%d", doc.ErrorID)
		}
		return doc.Balance, nil
	default:
		return 0, fmt.Errorf("неизвестный протокол %q", p.Protocol)
	}
}

// probeAIProvider does a light models-list call: openai dialects GET
// /models, anthropic GET /v1/models with the version header.
func probeAIProvider(ctx context.Context, p config.AIProvider) (string, string) {
	key := os.Getenv(p.APIKeyEnv)
	if key == "" {
		return "FAIL", "api_key_env " + p.APIKeyEnv + " не задан"
	}
	base := strings.TrimRight(p.BaseURL, "/")
	var url string
	headers := map[string]string{}
	switch p.Dialect {
	case ai.DialectAnthropicMessages:
		url = base + "/v1/models"
		if strings.HasSuffix(base, "/v1") {
			url = base + "/models"
		}
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	default: // both openai dialects share the models listing
		url = base + "/models"
		headers["Authorization"] = "Bearer " + key
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "FAIL", err.Error()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "FAIL", "недоступен: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "OK", fmt.Sprintf("%s: models-list отвечает", p.Dialect)
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return "FAIL", fmt.Sprintf("%s: ключ отклонён (HTTP %d)", p.Dialect, resp.StatusCode)
	default:
		return "WARN", fmt.Sprintf("%s: HTTP %d (base_url может не поддерживать models-list)", p.Dialect, resp.StatusCode)
	}
}
