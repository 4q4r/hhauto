package cli

// onboarding — the interactive first-run wizard. The terminal driver is
// intentionally thin: every decision/serialization step is an exported-to-
// test pure function in this file (renderProfileMD, defaultUA,
// accountTOMLBlock, parseCookiesInput, validAccountID).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/engine"
	"hhauto/internal/profile"
	"hhauto/internal/proxy"
	"hhauto/internal/store"
)

func newOnboardingCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "onboarding",
		Short: "Interactive first-run wizard: config skeleton, account, cookies, session check, profile.md",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOnboarding(cmd.Context(), outFor(cmd))
		},
	}
}

// interactive abstracts terminal input (tests never drive it; the brief
// keeps the wizard untested and the logic functions tested).
type interactive struct {
	in  *bufio.Reader
	out out
}

func newInteractive(o out) *interactive {
	return &interactive{in: bufio.NewReader(os.Stdin), out: o}
}

func (t *interactive) line(prompt string) string {
	t.out.printf("%s", prompt)
	s, err := t.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimSpace(s)
}

func (t *interactive) hidden(prompt string) string {
	t.out.printf("%s", prompt)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		// Piped input (scripted setup): fall back to a plain line read.
		s, _ := t.in.ReadString('\n')
		return strings.TrimSpace(s)
	}
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	t.out.println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (t *interactive) confirm(prompt string, defYes bool) bool {
	suffix := " [y/N] "
	if defYes {
		suffix = " [Y/n] "
	}
	ans := strings.ToLower(t.line(prompt + suffix))
	if ans == "" {
		return defYes
	}
	return ans == "y" || ans == "yes"
}

func runOnboarding(ctx context.Context, o out) error {
	t := newInteractive(o)

	// 1. Config skeleton.
	created, err := ensureConfigFile(flags.configPath)
	if err != nil {
		return exitErr("config: %v", err)
	}
	if created {
		o.printf("Создан скелет конфига: %s (заполните секции по необходимости)\n", flags.configPath)
	}
	cfg, err := LoadConfigOnly(flags.configPath)
	if err != nil {
		return exitErr("%v", err)
	}

	// 2. Account.
	if len(cfg.Accounts) > 0 && !t.confirm("Аккаунты уже настроены. Добавить ещё один?", false) {
		return nil
	}
	id, err := createAccountInteractive(t, o, cfg)
	if err != nil {
		return err
	}

	// Reload with the new account.
	cfg, err = LoadConfigOnly(flags.configPath)
	if err != nil {
		return exitErr("%v", err)
	}

	ckStore, err := resolveCookieStore(cfg)
	if err != nil {
		return exitErr("cookie store: %v", err)
	}

	// 3. Cookies (hidden input).
	o.println("Вставьте куки hh.ru одной строкой: \"name=value; name2=value2\", JSON или содержимое cookies.txt (ввод скрыт):")
	raw := t.hidden("> ")
	if err := saveCookiesInteractive(ctx, o, ckStore, id, raw); err != nil {
		return err
	}

	// 4. Session check + resume list (write-through to the store).
	if raw != "" {
		if err := checkSessionOnboarding(ctx, cfg, ckStore, id, o); err != nil {
			return err
		}
	}

	// 5. profile.md Q&A.
	if t.confirm("Заполнить profile.md (6 вопросов для ИИ-фактов)?", true) {
		if err := fillProfileInteractive(t, o, cfg, id); err != nil {
			return err
		}
	}

	o.println("\nДальнейшие шаги:")
	o.println("  1. hhauto config validate          — проверить конфиг")
	o.println("  2. hhauto account check " + id + "       — перепроверить сессию")
	o.println("  3. hhauto doctor                   — сеть, прокси, капча, ИИ")
	o.println("  4. hhauto run --once --dry-run     — прогон без мутаций")
	o.println("  5. hhauto run                      — боевой режим (Ctrl-C / STOP-файл для остановки)")
	return nil
}

// createAccountInteractive prompts for the new account fields, validates
// them against the config and appends the account to the TOML file.
func createAccountInteractive(t *interactive, o out, cfg *config.Config) (string, error) {
	id := t.line("ID аккаунта (латиницей, например main): ")
	if id == "" {
		return "", exitErr("пустой id аккаунта")
	}
	if !validAccountID(id) {
		return "", exitErr("id %q: допустимы латинские буквы, цифры, '.', '_', '-'", id)
	}
	if _, exists := cfg.Accounts[id]; exists {
		return "", exitErr("аккаунт %q уже есть в конфиге", id)
	}
	display := t.line("Отображаемое имя (Enter = " + id + "): ")
	proxyRef := t.line("proxy_ref (Enter = напрямую; доступные: " + proxyList(cfg) + "): ")
	if proxyRef != "" {
		if _, ok := cfg.Proxy[proxyRef]; !ok {
			return "", exitErr("proxy_ref %q не найден в [proxy.*]", proxyRef)
		}
	}
	ua := t.line("User-Agent (Enter = реалистичный Chrome по умолчанию): ")
	if ua == "" {
		ua = defaultUA()
	}

	// Persist the account into the TOML file.
	if err := appendAccountToTOML(flags.configPath, id, display, proxyRef, ua); err != nil {
		return "", exitErr("config update: %v", err)
	}
	o.printf("Аккаунт %s добавлен в %s\n", id, flags.configPath)
	return id, nil
}

// saveCookiesInteractive parses the pasted cookie string and saves it to the
// account store; empty input just prints the skip hint.
func saveCookiesInteractive(ctx context.Context, o out, ckStore cookies.Store, id, raw string) error {
	if raw == "" {
		o.println("Куки пропущены — добавьте позже: hhauto account add --id " + id + " --cookies-file cookies.txt")
		return nil
	}
	cks, err := parseCookiesInput(raw)
	if err != nil {
		return exitErr("%v", err)
	}
	if err := ckStore.Save(ctx, id, cks); err != nil {
		return exitErr("cookie save: %v", err)
	}
	o.printf("Сохранено кук: %d (backend: %s)\n", len(cks), ckStore.Backend())
	return nil
}

// checkSessionOnboarding opens the store, runs bootstrap+me for the new
// account and prints the outcome; a failed check is advisory, not fatal.
func checkSessionOnboarding(ctx context.Context, cfg *config.Config, ckStore cookies.Store, id string, o out) error {
	dbPath := cfg.Storage.Path
	if dbPath == "" {
		dbPath = filepath.Join(cfg.General.DataDir, "hhauto.db")
	}
	db, err := store.Open(ctx, dbPath, store.Options{BusyTimeoutMS: cfg.Storage.BusyTimeoutMS})
	if err != nil {
		return exitErr("store: %v", err)
	}
	checkErr := checkAccountSession(ctx, cfg, db, ckStore, id, o)
	_ = db.Close()
	if checkErr != nil {
		o.errf("Проверка сессии не прошла: %v (перелогиньтесь и повторите: hhauto account check %s)", checkErr, id)
	}
	return nil
}

// fillProfileInteractive asks the onboarding questions and writes profile.md.
func fillProfileInteractive(t *interactive, o out, cfg *config.Config, id string) error {
	questions := profile.SuggestOnboardingQuestions()
	answers := make([]string, len(questions))
	o.println("Отвечайте строкой на каждый вопрос (Enter — пропустить):")
	for i, q := range questions {
		answers[i] = t.line(q + "\n  → ")
	}
	path := profilePathFor(cfg, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return exitErr("profile dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(renderProfileMD(questions, answers)), 0o600); err != nil {
		return exitErr("profile.md write: %v", err)
	}
	o.println("Профиль сохранён:", path)
	return nil
}

// ensureConfigFile creates the commented skeleton when the file is missing.
func ensureConfigFile(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	skeleton := "# hhauto configuration — created by `hhauto onboarding`.\n" +
		"# The full commented reference lives below; every line is optional.\n\n" +
		"[general]\n" +
		"data_dir = \"~/.local/share/hhauto\"\n\n" +
		configTemplate
	return true, os.WriteFile(path, []byte(skeleton), 0o600)
}

// validAccountID mirrors the cookie-store identifier rule so an id accepted
// here can never fail at cookie persistence time.
func validAccountID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func proxyList(cfg *config.Config) string {
	if len(cfg.Proxy) == 0 {
		return "нет"
	}
	names := make([]string, 0, len(cfg.Proxy))
	for n := range cfg.Proxy {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// defaultUA proposes a realistic desktop Chrome UA (Linux; the host OS of
// the operator machine is what hh sees through the browser-parity headers).
func defaultUA() string {
	return "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
}

// renderProfileMD builds the profile.md document from the question set and
// the user's answers; unanswered questions are kept as prompts.
func renderProfileMD(questions, answers []string) string {
	var b strings.Builder
	b.WriteString("# Профиль кандидата (источник фактов для ИИ)\n\n")
	b.WriteString("Факты ниже — ЕДИНСТВЕННЫЙ источник внерезюменных фактов. ")
	b.WriteString("Пустые вопросы означают: ответь вручную или допиши сюда.\n\n")
	for i, q := range questions {
		b.WriteString("## " + q + "\n\n")
		if i < len(answers) && strings.TrimSpace(answers[i]) != "" {
			b.WriteString(strings.TrimSpace(answers[i]) + "\n\n")
		} else {
			b.WriteString("<!-- ответ не заполнен -->\n\n")
		}
	}
	return b.String()
}

// profilePathFor mirrors internal/profile's resolution: the account
// override wins, otherwise <data_dir>/profile.md.
func profilePathFor(cfg *config.Config, accountID string) string {
	if acct, ok := cfg.Accounts[accountID]; ok && acct.ProfileFile != "" {
		return config.ExpandPath(acct.ProfileFile)
	}
	return filepath.Join(cfg.General.DataDir, "profile.md")
}

// checkAccountSession runs BootstrapSession + Me and writes the resume
// snapshot through to the store (the onboarding proof of liveness).
func checkAccountSession(ctx context.Context, cfg *config.Config, db *store.DB, ckStore cookies.Store, id string, o out) error {
	reg, err := proxy.NewRegistry(cfg.Proxy)
	if err != nil {
		return err
	}
	apis, err := engine.BuildAPIClients(ctx, cfg, reg, ckStore, nil)
	if err != nil {
		return err
	}
	api, ok := apis[id]
	if !ok {
		return fmt.Errorf("нет клиента для аккаунта %s", id)
	}
	if err := api.BootstrapSession(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	u, err := api.Me(ctx)
	if err != nil {
		return fmt.Errorf("me: %w", err)
	}
	o.printf("Сессия жива: %s (%s, id %d)\n", u.FullName, u.Email, u.ID)

	resumes, err := api.ListResumes(ctx)
	if err != nil {
		return fmt.Errorf("list resumes: %w", err)
	}
	if err := db.AccountsUpsert(ctx, store.Account{ID: id}); err != nil {
		return fmt.Errorf("account upsert: %w", err)
	}
	stored := 0
	for _, r := range resumes {
		if r.HHID == "" {
			continue
		}
		row := store.Resume{
			AccountID: id, HhResumeID: r.HHID, Title: r.Title,
			Status: r.Status, Raw: string(r.Raw), UpdatedAt: r.UpdatedAt,
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = time.Now()
		}
		if err := db.ResumesUpsert(ctx, &row); err != nil {
			return fmt.Errorf("resume upsert: %w", err)
		}
		stored++
	}
	o.printf("Резюме в store: %d\n", stored)
	for _, r := range resumes {
		o.printf("  - %s  %s  (%s)\n", r.HHID, r.Title, r.Status)
	}
	return nil
}
