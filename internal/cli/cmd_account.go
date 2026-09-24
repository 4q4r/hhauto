package cli

// account add|list|check|remove — account lifecycle. The TOML file is the
// source of truth for accounts; edits append or remove the
// [accounts.<id>] table textually (a new table header is valid anywhere at
// top level, so appending is structurally safe).

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"hhauto/internal/config"
)

// accountBlock is the marshaled per-account TOML fragment (only the fields
// onboarding/add set; the rest keeps the documented defaults).
type accountBlock struct {
	DisplayName string `toml:"display_name,omitempty"`
	ProxyRef    string `toml:"proxy_ref,omitempty"`
	UserAgent   string `toml:"user_agent,omitempty"`
}

// accountTOMLBlock renders the [accounts.<id>] table. go-toml marshals a
// single-entry map as "[<id>]" — the header is rewritten to the fully
// qualified "[accounts.<id>]" so the fragment can be appended verbatim.
func accountTOMLBlock(id, display, proxyRef, ua string) (string, error) {
	b, err := toml.Marshal(map[string]accountBlock{id: {
		DisplayName: display, ProxyRef: proxyRef, UserAgent: ua,
	}})
	if err != nil {
		return "", err
	}
	s := string(b)
	old := "[" + id + "]"
	if !strings.HasPrefix(s, old) {
		return "", fmt.Errorf("unexpected marshal output for account %q", id)
	}
	return "[accounts." + id + "]" + s[len(old):], nil
}

// appendAccountToTOML appends the account table, refusing duplicates.
func appendAccountToTOML(path, id, display, proxyRef, ua string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := config.Load(path); err != nil {
		return fmt.Errorf("existing config is invalid; fix it first: %w", err)
	}
	header := "[accounts." + id + "]"
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == header {
			return fmt.Errorf("аккаунт %q уже описан в конфиге", id)
		}
	}
	block, err := accountTOMLBlock(id, display, proxyRef, ua)
	if err != nil {
		return err
	}
	out := strings.TrimRight(string(raw), "\n") + "\n\n" + block + "\n"
	return os.WriteFile(path, []byte(out), 0o600)
}

// removeAccountSection drops the [accounts.<id>] table and its sub-tables
// ([accounts.<id>.headers] etc.) from the file: from the header to the next
// top-level table header that is not a sub-table of the same account.
func removeAccountSection(path, id string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	header := "[accounts." + id + "]"
	subPrefix := "[accounts." + id + "."
	lines := strings.Split(string(raw), "\n")
	var out []string
	removed := false
	inSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isTableHeader := strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]")
		if !inSection && trimmed == header {
			inSection = true
			removed = true
			continue
		}
		if inSection {
			endsSection := isTableHeader && !strings.HasPrefix(trimmed, subPrefix)
			if endsSection {
				inSection = false
			} else {
				continue
			}
		}
		out = append(out, line)
	}
	if !removed {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600)
}

func newAccountCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Manage hh.ru accounts (add | list | check | remove)"}
	cmd.AddCommand(
		newAccountAddCmd(), newAccountListCmd(),
		newAccountCheckCmd(), newAccountRemoveCmd(),
	)
	return cmd
}

func newAccountAddCmd() *cobra.Command {
	var (
		id          string
		proxyRef    string
		cookiesFile string
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add an account (optionally importing cookies from cookies.txt or a JSON export)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id == "" {
				return exitErr("--id is required")
			}
			if !validAccountID(id) {
				return exitErr("id %q: допустимы латинские буквы, цифры, '.', '_', '-'", id)
			}
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("%v", err)
			}
			if err := validateNewAccount(cfg, id, proxyRef); err != nil {
				return err
			}
			if err := appendAccountToTOML(flags.configPath, id, "", proxyRef, ""); err != nil {
				return exitErr("%v", err)
			}

			var imported int
			if cookiesFile != "" {
				var ierr error
				imported, ierr = importCookiesFile(cmd.Context(), cfg, id, cookiesFile)
				if ierr != nil {
					return ierr
				}
			}
			o := outFor(cmd)
			o.printf("Аккаунт %s добавлен", id)
			if imported > 0 {
				o.printf(", кук импортировано: %d", imported)
			}
			o.println()
			o.println("Проверка: hhauto account check " + id)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "account id (latin letters, digits, . _ -)")
	cmd.Flags().StringVar(&proxyRef, "proxy", "", "proxy reference from [proxy.*]")
	cmd.Flags().StringVar(&cookiesFile, "cookies-file", "", "Netscape cookies.txt or JSON export to import")
	return cmd
}

// validateNewAccount rejects malformed ids, ids already present in the
// config and proxy refs not defined in [proxy.*].
func validateNewAccount(cfg *config.Config, id, proxyRef string) error {
	if _, exists := cfg.Accounts[id]; exists {
		return exitErr("аккаунт %q уже есть в конфиге", id)
	}
	if proxyRef != "" {
		if _, ok := cfg.Proxy[proxyRef]; !ok {
			return exitErr("proxy_ref %q не найден в [proxy.*]", proxyRef)
		}
	}
	return nil
}

// importCookiesFile parses the cookies export and saves it to the account
// cookie store; returns the number of imported cookies.
func importCookiesFile(ctx context.Context, cfg *config.Config, id, cookiesFile string) (int, error) {
	raw, err := os.ReadFile(cookiesFile)
	if err != nil {
		return 0, exitErr("cookies file: %v", err)
	}
	cks, err := parseCookiesInput(string(raw))
	if err != nil {
		return 0, exitErr("%v", err)
	}
	ckStore, err := resolveCookieStore(cfg)
	if err != nil {
		return 0, exitErr("cookie store: %v", err)
	}
	if err := ckStore.Save(ctx, id, cks); err != nil {
		return 0, exitErr("cookie save: %v", err)
	}
	return len(cks), nil
}

func newAccountListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured accounts with their store status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("%v", err)
			}
			o := outFor(cmd)
			ids := make([]string, 0, len(cfg.Accounts))
			for id := range cfg.Accounts {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) == 0 {
				o.println("Аккаунтов нет. Начните с hhauto onboarding.")
				return nil
			}
			rows := make([][]string, 0, len(ids))
			for _, id := range ids {
				a := cfg.Accounts[id]
				rows = append(rows, []string{id, a.DisplayName, a.ProxyRef, truncateUA(a.UserAgent)})
			}
			o.printTable([]string{"ID", "NAME", "PROXY", "USER-AGENT"}, rows)
			return nil
		},
	}
}

func truncateUA(ua string) string {
	if len(ua) <= 40 {
		return ua
	}
	return ua[:37] + "..."
}

func newAccountCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "check [ID]",
		Short:              "Verify a session live: bootstrap + Me, resumes to store",
		Args:               cobra.ExactArgs(1),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			ctx := cmd.Context()
			rt, err := Build(ctx, buildOpts())
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			if _, ok := rt.Cfg.Accounts[id]; !ok {
				return exitErr("аккаунт %q не найден", id)
			}
			if err := checkAccountSession(ctx, rt.Cfg, rt.DB, rt.Cookies, id, outFor(cmd)); err != nil {
				return exitErr("%v", err)
			}
			return nil
		},
	}
}

func newAccountRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove ID",
		Short: "Remove an account from the config and delete its stored cookies",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				return exitErr("отказ: удаление требует явного --yes")
			}
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("%v", err)
			}
			if _, ok := cfg.Accounts[id]; !ok {
				return exitErr("аккаунт %q не найден", id)
			}
			removed, err := removeAccountSection(flags.configPath, id)
			if err != nil {
				return exitErr("config update: %v", err)
			}
			if !removed {
				return exitErr("секция [accounts.%s] не найдена в файле", id)
			}
			ckStore, err := resolveCookieStore(cfg)
			if err == nil {
				ctx := cmd.Context()
				if derr := ckStore.Delete(ctx, id); derr != nil && !strings.Contains(derr.Error(), "not found") {
					outFor(cmd).errf("куки не удалены: %v", derr)
				}
			}
			outFor(cmd).printf("Аккаунт %s удалён\n", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm removal")
	return cmd
}
