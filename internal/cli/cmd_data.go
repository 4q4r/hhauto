package cli

// Data-plane commands: vacancies search (read-only cascade), resume,
// chats, contacts export, tests, review, profile edit.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"hhauto/internal/config"
	"hhauto/internal/features/emplrating"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/relevance"
	"hhauto/internal/features/testsolve"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/store"
)

func newVacanciesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "vacancies", Short: "Vacancy discovery (read-only preview of the apply cascade)"}
	cmd.AddCommand(newVacanciesSearchCmd())
	return cmd
}

func newVacanciesSearchCmd() *cobra.Command {
	var (
		profileID string
		limit     int
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Run the source + filter cascade WITHOUT applying: table of vacancy, employer, score/band, stage/skip-reason",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profileID == "" {
				return exitErr("--profile is required")
			}
			if limit <= 0 {
				limit = 25
			}
			opts := buildOpts()
			opts.Profile = profileID
			if dryRun {
				opts.DryRun = true
			}
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			run, ok := rt.Cfg.Runs[profileID]
			if !ok {
				return exitErr("run profile %q не найден", profileID)
			}
			return vacanciesSearch(ctx, rt, outFor(cmd), profileID, run, limit)
		},
	}
	cmd.Flags().StringVar(&profileID, "profile", "", "run profile id from [runs.*]")
	cmd.Flags().IntVar(&limit, "limit", 25, "max vacancies to examine")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "accepted for symmetry; search never applies")
	return cmd
}

// vacancyRow is one table line of the search preview (--json too).
type vacancyRow struct {
	VacancyID  int64  `json:"vacancy_id"`
	Name       string `json:"name"`
	Employer   string `json:"employer"`
	Score      int    `json:"score,omitempty"`
	Band       string `json:"band,omitempty"`
	Stage      string `json:"stage"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// vacanciesSearch runs the discovery + cascade in read-only mode. It mirrors
// multiapply's stage order (F2..F7) using the feature packages' exported
// filters (relevance, emplrating); the tiny pure F2/F3 checks are local
// copies of multiapply's logic (multiapply keeps them private). Nothing is
// ever applied here: the cascade stops before F8 by construction.
func vacanciesSearch(ctx context.Context, rt *Runtime, o out, runID string, run config.Run, limit int) error {
	accountID := run.Account
	api, err := rt.APIFor(accountID)
	if err != nil {
		return exitErr("api: %v", err)
	}
	resumeHash, err := resolveResumeHash(ctx, rt, accountID, api, run)
	if err != nil {
		return exitErr("resume: %v", err)
	}
	prof, err := rt.profileFor(ctx, accountID)
	if err != nil {
		return exitErr("profile: %v", err)
	}

	vacancies, err := collectVacancies(ctx, api, run, resumeHash, limit)
	if err != nil {
		return exitErr("collect: %v", err)
	}

	flt := run.Filters
	var rows []vacancyRow
	for _, v := range vacancies {
		if len(rows) >= limit {
			break
		}
		_, _ = rt.DB.VacanciesSeenUpsert(ctx, store.VacancySeen{
			AccountID: accountID, VacancyID: v.ID, Source: "cli-search",
			FirstSeenAt: time.Now().UTC(), Meta: runID,
		})
		row := vacancyRow{VacancyID: v.ID, Name: v.Name, Employer: v.EmployerName, Stage: "F1_source"}

		switch {
		case excludedWords(v, flt):
			row.SkipReason = "exclude_words"
		case whitelisted(flt) && !matchEmployer(flt.CompanyWhitelist, v.EmployerID, v.EmployerName):
			row.SkipReason = "not_whitelisted"
		case matchEmployer(flt.CompanyBlacklist, v.EmployerID, v.EmployerName):
			row.SkipReason = "blacklisted"
		default:
			row = screenVacancy(ctx, rt, run, runID, prof, resumeHash, api, v, row)
		}
		rows = append(rows, row)
	}

	if flags.json {
		return o.printJSON(rows)
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			strconv.FormatInt(r.VacancyID, 10), clip(r.Name, 40), clip(r.Employer, 24),
			orDash(r.Band), orDash(strconv.Itoa(r.Score)), r.Stage, orDash(r.SkipReason),
		})
	}
	o.printTable([]string{"VACANCY", "NAME", "EMPLOYER", "BAND", "SCORE", "STAGE", "SKIP"}, table)
	o.printf("\nПоказано: %d (лимит %d). Отклики НЕ отправляются.\n", len(rows), limit)
	return nil
}

// screenVacancyAI applies the AI relevance screen (F6) when the AI filter
// is enabled and a runner is wired; otherwise the row passes through.
func screenVacancyAI(ctx context.Context, rt *Runtime, run config.Run, runID string,
	prof profile.Profile, resumeHash string, v hhapi.Vacancy, row vacancyRow) vacancyRow {
	if rt.Cfg.Features.MultiApply.AIFilter && rt.AI != nil {
		threshold := run.AIFilterOverrides.MinScore
		if threshold <= 0 {
			threshold = rt.Cfg.Features.MultiApply.MinMatchPercent
		}
		if threshold <= 0 {
			threshold = 60
		}
		verdict, pass, reason := relevance.New(rt.AI, rt.DB, run.Account, runID, prof, relevance.Rules{
			ExcludeWords:      run.Filters.ExcludeWords,
			TargetRoles:       relevance.RolesFromFacts(prof),
			ExtraInstructions: run.AIFilterOverrides.ExtraInstructions,
		}, rt.Log).Pass(ctx, v, resumeHash, threshold)
		row.Score, row.Band = verdict.Score, verdict.Band
		if !pass {
			row.SkipReason = reason
			return row
		}
	}
	return row
}

// screenVacancy runs the F4-F7 preview cascade for one vacancy; a preview
// stops before F8 by construction. Every skip keeps the row (with only
// SkipReason set), so the caller always appends the row.
func screenVacancy(ctx context.Context, rt *Runtime, run config.Run, runID string,
	prof profile.Profile, resumeHash string, api hhapi.HHAPI, v hhapi.Vacancy, row vacancyRow) vacancyRow {
	row.Stage = "F4_rating"
	if pass, reason := emplrating.New(rt.DB, api, rt.Log).Pass(ctx, run.Account, v.EmployerID, run.Filters); !pass {
		row.SkipReason = reason
		return row
	}
	row.Stage = "F5_dedup"
	key := featurekit.DedupKey(run.Account, resumeHash, v.ID)
	if app, derr := rt.DB.ApplicationsGetByDedupKey(ctx, key); derr == nil && app != nil {
		row.SkipReason = "already_applied"
		return row
	}
	if rt.Cfg.Features.MultiApply.DedupNativeResp {
		if accepts, found := acceptsAutoResponse(v.Raw); found && !accepts {
			row.SkipReason = "native_autoresponse"
			return row
		}
	}
	row = screenVacancyAI(ctx, rt, run, runID, prof, resumeHash, v, row)
	row.Stage = "F6.5_popup"
	meta, perr := api.ApplyPopup(ctx, v.ID)
	if perr != nil {
		row.SkipReason = "popup_failed"
		return row
	}
	if len(meta.ResumeInconsistencies) > 0 {
		row.SkipReason = "inconsistencies"
		return row
	}
	if meta.ChatWritePossibility == "DISABLED" {
		row.SkipReason = "chat_disabled"
		return row
	}
	row.Stage = "F7_quota"
	if run.MaxPerDay > 0 || rt.Cfg.Features.MultiApply.Enabled {
		// Quota numbers come from the store counters; a preview never
		// crosses into F8 regardless of their values.
		row.Stage = "F8_apply_would_run"
	}
	return row
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// resolveResumeHash picks the run resume: the configured hash or the first
// published+searchable snapshot (refreshing through the API when empty).
func resolveResumeHash(ctx context.Context, rt *Runtime, accountID string, api hhapi.HHAPI, run config.Run) (string, error) {
	if run.ResumeID != "" {
		return run.ResumeID, nil
	}
	rows, err := rt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	if h := firstPublished(rows); h != "" {
		return h, nil
	}
	got, err := api.ListResumes(ctx)
	if err != nil {
		return "", err
	}
	if err := rt.DB.AccountsUpsert(ctx, store.Account{ID: accountID}); err != nil {
		return "", err
	}
	for _, r := range got {
		if r.HHID == "" {
			continue
		}
		row := store.Resume{
			AccountID: accountID, HhResumeID: r.HHID, Title: r.Title,
			Status: r.Status, Raw: string(r.Raw), UpdatedAt: r.UpdatedAt,
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = time.Now()
		}
		if err := rt.DB.ResumesUpsert(ctx, &row); err != nil {
			return "", err
		}
	}
	rows, err = rt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	if h := firstPublished(rows); h != "" {
		return h, nil
	}
	return "", fmt.Errorf("нет опубликованного резюме")
}

func firstPublished(rows []store.Resume) string {
	for _, r := range rows {
		s := strings.ToLower(strings.TrimSpace(r.Status))
		if s == "published" || s == "опубликовано" {
			return r.HhResumeID
		}
	}
	return ""
}

// collectVacancies walks the run's source (search or suitable-vacancies)
// with a page cap, stopping early once the limit is reached.
func collectVacancies(ctx context.Context, api hhapi.HHAPI, run config.Run, resumeHash string, limit int) ([]hhapi.Vacancy, error) {
	const maxPages = 5
	var out []hhapi.Vacancy
	if run.Query == "" {
		for page := 0; page < maxPages; page++ {
			vp, err := api.SuitableVacancies(ctx, resumeHash, page)
			if err != nil {
				return out, err
			}
			out = append(out, vp.Items...)
			if len(out) >= limit || page+1 >= vp.Pages || len(vp.Items) == 0 {
				break
			}
		}
		return out, nil
	}
	flt := run.Filters
	q := hhapi.SearchQuery{
		Text:            run.Query,
		Areas:           flt.Areas,
		Experience:      flt.Experience,
		Schedule:        flt.Schedule,
		Employment:      flt.Employment,
		Industries:      flt.Industries,
		SalaryFrom:      flt.SalaryFrom,
		Currency:        flt.SalaryCurrency,
		LabelNoAgencies: flt.ExcludeAgencies,
		SearchMode:      run.SearchMode,
		PeriodDays:      flt.SearchPeriodDays,
		PerPage:         50,
	}
	for page := 0; page < maxPages; page++ {
		q.Page = page
		vp, err := api.SearchVacancies(ctx, q)
		if err != nil {
			return out, err
		}
		out = append(out, vp.Items...)
		if len(out) >= limit || page+1 >= vp.Pages || len(vp.Items) == 0 {
			break
		}
	}
	return out, nil
}

// excludedWords is the F2 check (multiapply-local logic mirrored for the
// read-only preview).
func excludedWords(v hhapi.Vacancy, flt config.RunFilters) bool {
	name := strings.ToLower(v.Name)
	desc := ""
	if flt.ExcludeInDesc {
		desc = strings.ToLower(v.Description)
	}
	for _, w := range flt.ExcludeWords {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if strings.Contains(name, w) || (desc != "" && strings.Contains(desc, w)) {
			return true
		}
	}
	return false
}

func whitelisted(flt config.RunFilters) bool { return len(flt.CompanyWhitelist) > 0 }

func matchEmployer(list []string, empID int64, empName string) bool {
	id := strconv.FormatInt(empID, 10)
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == id || strings.EqualFold(entry, empName) {
			return true
		}
	}
	return false
}

// acceptsAutoResponse reads acceptAutoResponse from the vacancy raw JSON
// (top-level or nested under autoResponse); found=false when absent.
func acceptsAutoResponse(raw json.RawMessage) (accepts, found bool) {
	if len(raw) == 0 {
		return true, false
	}
	var probe struct {
		AcceptAutoResponse *bool `json:"acceptAutoResponse"`
		AutoResponse       *struct {
			AcceptAutoResponse *bool `json:"acceptAutoResponse"`
		} `json:"autoResponse"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return true, false
	}
	switch {
	case probe.AcceptAutoResponse != nil:
		return *probe.AcceptAutoResponse, true
	case probe.AutoResponse != nil && probe.AutoResponse.AcceptAutoResponse != nil:
		return *probe.AutoResponse.AcceptAutoResponse, true
	default:
		return true, false
	}
}

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "resume", Short: "Resume operations (list | lift)"}
	cmd.AddCommand(newResumeListCmd(), newResumeLiftCmd())
	return cmd
}

func newResumeListCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored resume snapshots",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			o := outFor(cmd)
			type row struct {
				Account string `json:"account"`
				Hash    string `json:"hash"`
				Title   string `json:"title"`
				Status  string `json:"status"`
				Updated string `json:"updated_at"`
			}
			var rows []row
			for id := range rt.Cfg.Accounts {
				if account != "" && id != account {
					continue
				}
				items, err := rt.DB.ResumesListByAccount(ctx, id)
				if err != nil {
					return exitErr("resumes %s: %v", id, err)
				}
				for _, r := range items {
					rows = append(rows, row{Account: id, Hash: r.HhResumeID, Title: r.Title,
						Status: r.Status, Updated: r.UpdatedAt.Format("2006-01-02 15:04")})
				}
			}
			if flags.json {
				return o.printJSON(rows)
			}
			if len(rows) == 0 {
				o.println("В store нет резюме. Запустите hhauto onboarding или hhauto account check <ID>.")
				return nil
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				table = append(table, []string{r.Account, r.Hash, clip(r.Title, 40), r.Status, r.Updated})
			}
			o.printTable([]string{"ACCOUNT", "HASH", "TITLE", "STATUS", "UPDATED"}, table)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	return cmd
}

func newResumeLiftCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "lift",
		Short: "Lift (touch) one resume now",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitErr("использование: hhauto resume lift RESUME_HASH")
			}
			hash := args[0]
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			if len(rt.Cfg.Accounts) == 0 {
				return exitErr("аккаунтов нет")
			}
			target := account
			if target == "" {
				for id := range rt.Cfg.Accounts {
					target = id
					break
				}
			}
			api, err := rt.APIFor(target)
			if err != nil {
				return exitErr("api: %v", err)
			}
			o := outFor(cmd)
			if rt.Cfg.General.DryRun {
				o.printf("dry-run: lift %s пропущен (мутация не выполнялась)\n", hash)
				return nil
			}
			if err := rt.Pacer.Wait(ctx, target); err != nil {
				return exitErr("pacer: %v", err)
			}
			if err := api.TouchResume(ctx, hash); err != nil {
				return exitErr("lift: %v", err)
			}
			o.printf("Резюме %s поднято (аккаунт %s)\n", hash, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "account id (required when several are configured)")
	return cmd
}

func newChatsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "chats", Short: "Chat operations (list | show | reply | leave)"}
	cmd.AddCommand(newChatsListCmd(), newChatsShowCmd(), newChatsReplyCmd(), newChatsLeaveCmd())
	return cmd
}

func newChatsListCmd() *cobra.Command {
	var (
		account string
		state   string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List live chats (optionally filtered by the chatops state mirror)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			o := outFor(cmd)
			type row struct {
				Account  string `json:"account"`
				Chat     string `json:"chat"`
				State    string `json:"state"`
				Employer string `json:"employer,omitempty"`
				Unread   int    `json:"unread,omitempty"`
			}
			var rows []row
			for id := range rt.Cfg.Accounts {
				if account != "" && id != account {
					continue
				}
				api, aerr := rt.APIFor(id)
				if aerr != nil {
					return exitErr("api %s: %v", id, aerr)
				}
				chats, lerr := api.ListChats(ctx, 1)
				if lerr != nil {
					return exitErr("chats %s: %v", id, lerr)
				}
				for _, c := range chats {
					st := chatMirrorState(ctx, rt, id, c.ID)
					if state != "" && st != state {
						continue
					}
					rows = append(rows, row{Account: id, Chat: c.ID, State: st, Employer: c.EmployerName, Unread: c.Unread})
				}
			}
			if flags.json {
				return o.printJSON(rows)
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				table = append(table, []string{r.Account, r.Chat, r.State, clip(r.Employer, 30), strconv.Itoa(r.Unread)})
			}
			o.printTable([]string{"ACCOUNT", "CHAT", "STATE", "EMPLOYER", "UNREAD"}, table)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	cmd.Flags().StringVar(&state, "state", "", "filter by chatops state (active|rejected|left|escalated|warmup_due)")
	return cmd
}

// chatMirrorState reads the chatops state mirror for a chat (feature_state);
// chats never synced default to "active" (chatops' own default).
func chatMirrorState(ctx context.Context, rt *Runtime, accountID, chatikID string) string {
	val, ok, err := rt.DB.FeatureStateGet(ctx, "chatops", accountID, "state:"+chatikID)
	if err != nil || !ok || val == "" {
		return "active"
	}
	return val
}

func requireSingleAccount(rt *Runtime, account string) (string, error) {
	if account != "" {
		if _, ok := rt.Cfg.Accounts[account]; !ok {
			return "", fmt.Errorf("аккаунт %q не найден", account)
		}
		return account, nil
	}
	if len(rt.Cfg.Accounts) == 1 {
		for id := range rt.Cfg.Accounts {
			return id, nil
		}
	}
	return "", fmt.Errorf("укажите --account (настроено несколько аккаунтов)")
}

func newChatsShowCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "show CHAT",
		Short: "Print the chat history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			chatID := args[0]
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			id, err := requireSingleAccount(rt, account)
			if err != nil {
				return exitErr("%v", err)
			}
			api, err := rt.APIFor(id)
			if err != nil {
				return exitErr("api: %v", err)
			}
			msgs, err := api.ChatHistory(ctx, chatID)
			if err != nil {
				return exitErr("history: %v", err)
			}
			sort.Slice(msgs, func(i, j int) bool { return msgs[i].SentAt.Before(msgs[j].SentAt) })
			o := outFor(cmd)
			for _, m := range msgs {
				o.printf("%s [%s] %s: %s\n", m.SentAt.Format("01-02 15:04"), m.Direction, m.Author, strings.ReplaceAll(m.Text, "\n", " "))
			}
			if len(msgs) == 0 {
				o.println("(пусто)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "account id")
	return cmd
}

func newChatsReplyCmd() *cobra.Command {
	var (
		account string
		text    string
	)
	cmd := &cobra.Command{
		Use:   "reply CHAT",
		Short: "Send one message into a chat (link rule + dry-run respected)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			chatID := args[0]
			if strings.TrimSpace(text) == "" {
				return exitErr("--text is required")
			}
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			id, err := requireSingleAccount(rt, account)
			if err != nil {
				return exitErr("%v", err)
			}
			if err := featurekit.NoLinksRule(text); err != nil {
				return exitErr("%v (правило платформы: без ссылок)", err)
			}
			api, err := rt.APIFor(id)
			if err != nil {
				return exitErr("api: %v", err)
			}
			o := outFor(cmd)
			if rt.Cfg.General.DryRun {
				o.printf("dry-run: сообщение в %s не отправлено\n", chatID)
				return nil
			}
			if err := rt.Pacer.Wait(ctx, id); err != nil {
				return exitErr("pacer: %v", err)
			}
			key := "cli:" + ulid.Make().String()
			if err := api.SendMessage(ctx, chatID, key, text); err != nil {
				return exitErr("send: %v", err)
			}
			o.printf("Отправлено в %s (аккаунт %s)\n", chatID, id)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "account id")
	cmd.Flags().StringVar(&text, "text", "", "message text (links are forbidden)")
	return cmd
}

func newChatsLeaveCmd() *cobra.Command {
	var (
		account string
		yes     bool
	)
	cmd := &cobra.Command{
		Use:   "leave CHAT",
		Short: "Leave a chat",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			chatID := args[0]
			if !yes {
				return exitErr("отказ: выход из чата требует явного --yes")
			}
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			id, err := requireSingleAccount(rt, account)
			if err != nil {
				return exitErr("%v", err)
			}
			api, err := rt.APIFor(id)
			if err != nil {
				return exitErr("api: %v", err)
			}
			o := outFor(cmd)
			if rt.Cfg.General.DryRun {
				o.printf("dry-run: выход из %s не выполнен\n", chatID)
				return nil
			}
			if err := api.LeaveChat(ctx, chatID); err != nil {
				return exitErr("leave: %v", err)
			}
			_ = rt.DB.ChatsUpdateState(ctx, id, chatID, "left", nil)
			o.printf("Чат %s покинут (аккаунт %s)\n", chatID, id)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "account id")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm leaving")
	return cmd
}

func newContactsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "contacts", Short: "Employer contacts"}
	cmd.AddCommand(newContactsExportCmd())
	return cmd
}

func newContactsExportCmd() *cobra.Command {
	var (
		account string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export collected contacts as CSV or JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "csv" && format != "json" {
				return exitErr("--format must be csv or json")
			}
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			o := outFor(cmd)
			ids := make([]string, 0, len(rt.Cfg.Accounts))
			for id := range rt.Cfg.Accounts {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			type exportRow struct {
				Account    string   `json:"account"`
				EmployerID int64    `json:"employer_id"`
				Source     string   `json:"source"`
				Emails     []string `json:"emails"`
				Phones     []string `json:"phones"`
				Messengers []string `json:"messengers"`
			}
			var all []exportRow
			for _, id := range ids {
				if account != "" && id != account {
					continue
				}
				items, err := rt.DB.ContactsExport(ctx, id)
				if err != nil {
					return exitErr("contacts %s: %v", id, err)
				}
				for _, c := range items {
					row := exportRow{Account: id, EmployerID: c.EmployerID, Source: c.Source,
						Emails: []string{}, Phones: []string{}, Messengers: []string{}}
					_ = json.Unmarshal([]byte(c.Emails), &row.Emails)
					_ = json.Unmarshal([]byte(c.Phones), &row.Phones)
					_ = json.Unmarshal([]byte(c.Messengers), &row.Messengers)
					all = append(all, row)
				}
			}
			if format == "json" || flags.json {
				return o.printJSON(all)
			}
			o.println("account,employer_id,source,emails,phones,messengers")
			for _, r := range all {
				o.printf("%s,%d,%s,%q,%q,%q\n", r.Account, r.EmployerID, r.Source,
					strings.Join(r.Emails, " "), strings.Join(r.Phones, " "), strings.Join(r.Messengers, " "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	cmd.Flags().StringVar(&format, "format", "csv", "csv | json")
	return cmd
}

func newTestsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tests", Short: "Test assessments (list | show | open | solve)"}
	cmd.AddCommand(newTestsListCmd(), newTestsShowCmd(), newTestsOpenCmd(), newTestsSolveCmd())
	return cmd
}

// assessmentRow is the tests list row (--json too).
type assessmentRow struct {
	ID       int64  `json:"id"`
	Account  string `json:"account"`
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitempty"`
	URL      string `json:"url,omitempty"`
	Status   string `json:"status"`
	Created  string `json:"created_at"`
}

func listAssessments(ctx context.Context, rt *Runtime, accountFilter string) ([]assessmentRow, error) {
	ids := make([]string, 0, len(rt.Cfg.Accounts))
	for id := range rt.Cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var rows []assessmentRow
	for _, id := range ids {
		if accountFilter != "" && id != accountFilter {
			continue
		}
		items, err := rt.DB.AssessmentsListByAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, a := range items {
			row := assessmentRow{ID: a.ID, Account: id, Kind: a.Kind, Status: a.Status,
				Created: a.CreatedAt.Format("2006-01-02 15:04")}
			if a.Provider != nil {
				row.Provider = *a.Provider
			}
			if a.ExternalURL != nil {
				row.URL = *a.ExternalURL
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func newTestsListCmd() *cobra.Command {
	var account string
	return &cobra.Command{
		Use:   "list",
		Short: "List test assessments",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			rows, err := listAssessments(ctx, rt, account)
			if err != nil {
				return exitErr("%v", err)
			}
			o := outFor(cmd)
			if flags.json {
				return o.printJSON(rows)
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				table = append(table, []string{strconv.FormatInt(r.ID, 10), r.Account, r.Kind, orDash(r.Provider), r.Status, clip(r.URL, 48)})
			}
			o.printTable([]string{"ID", "ACCOUNT", "KIND", "PROVIDER", "STATUS", "URL"}, table)
			return nil
		},
	}
}

func findAssessment(ctx context.Context, rt *Runtime, accountFilter, idArg string) (*store.Assessment, string, error) {
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("ID должен быть числом")
	}
	ids := make([]string, 0, len(rt.Cfg.Accounts))
	for acc := range rt.Cfg.Accounts {
		ids = append(ids, acc)
	}
	sort.Strings(ids)
	for _, acc := range ids {
		if accountFilter != "" && acc != accountFilter {
			continue
		}
		items, err := rt.DB.AssessmentsListByAccount(ctx, acc)
		if err != nil {
			return nil, "", err
		}
		for i := range items {
			if items[i].ID == id {
				return &items[i], acc, nil
			}
		}
	}
	return nil, "", fmt.Errorf("тест %s не найден среди аккаунтов", idArg)
}

func newTestsShowCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Show one assessment with its answers artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			a, acc, err := findAssessment(ctx, rt, account, args[0])
			if err != nil {
				return exitErr("%v", err)
			}
			o := outFor(cmd)
			o.printf("ID: %d  аккаунт: %s  вид: %s  статус: %s\n", a.ID, acc, a.Kind, a.Status)
			if a.Provider != nil {
				o.println("Провайдер:", *a.Provider)
			}
			if a.ExternalURL != nil {
				o.println("URL:", *a.ExternalURL)
			}
			if a.Result != nil {
				o.println("Результат:", *a.Result)
			}
			if a.Answers != nil {
				buf := &strings.Builder{}
				if err := indentJSON(*a.Answers, buf); err == nil {
					o.println("Ответы:")
					o.println(buf.String())
				} else {
					o.println("Ответы:", *a.Answers)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	return cmd
}

// indentJSON re-indents a JSON string into w.
func indentJSON(s string, w *strings.Builder) error {
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	w.Write(b)
	return nil
}

func newTestsOpenCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "open ID",
		Short: "Print the external form URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			a, _, err := findAssessment(ctx, rt, account, args[0])
			if err != nil {
				return exitErr("%v", err)
			}
			if a.ExternalURL == nil || *a.ExternalURL == "" {
				return exitErr("у теста нет внешнего URL")
			}
			outFor(cmd).println(*a.ExternalURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	return cmd
}

func newTestsSolveCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "solve ID",
		Short: "Force the solve branch for an external form (draft_only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			a, acc, err := findAssessment(ctx, rt, account, args[0])
			if err != nil {
				return exitErr("%v", err)
			}
			if a.ExternalURL == nil || *a.ExternalURL == "" {
				return exitErr("у теста нет внешнего URL (solve применим только к внешним формам)")
			}
			// Force: retire the existing draft so the dedup gate re-runs.
			_ = rt.DB.AssessmentsUpdateStatus(ctx, a.ID, "detected", nil)
			solver := &testsolve.Solver{
				Cfg: *rt.Cfg, DB: rt.DB, AI: rt.AI, Notify: rt.Webhooks,
				Dialer: rt.dialerFor, Log: rt.Log, Queue: rt.Queue,
			}
			task := queue.Task{
				AccountID: acc, Kind: queue.KindSolveTest, Priority: queue.PrioChats,
				Payload: testsolve.SolvePayload{
					AccountID: acc, TestID: testsolve.TestIDForURL(*a.ExternalURL),
					URL: *a.ExternalURL, Source: "cli_solve",
				},
			}
			if err := solver.HandleSolveTest(ctx, task); err != nil {
				return exitErr("solve: %v", err)
			}
			outFor(cmd).println("Черновик сохранён в tests (hhauto tests list).")
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	return cmd
}

func newReviewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "review", Short: "Human review queue (list | show | resolve | ignore)"}
	cmd.AddCommand(newReviewListCmd(), newReviewShowCmd(), newReviewResolveCmd(), newReviewIgnoreCmd())
	return cmd
}

// reviewRows lists open items per account. The store exposes open items
// only (ReviewItemsOpen); closed history stays in the DB audit trail.
func reviewRows(ctx context.Context, rt *Runtime, accountFilter string) ([]store.ReviewItem, error) {
	ids := make([]string, 0, len(rt.Cfg.Accounts))
	for id := range rt.Cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var rows []store.ReviewItem
	for _, id := range ids {
		if accountFilter != "" && id != accountFilter {
			continue
		}
		open, err := rt.DB.ReviewItemsOpen(ctx, id, 200)
		if err != nil {
			return nil, err
		}
		rows = append(rows, open...)
	}
	return rows, nil
}

func newReviewListCmd() *cobra.Command {
	var (
		account string
		open    bool
	)
	return &cobra.Command{
		Use:   "list",
		Short: "List open review items",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = open // the store contract exposes open items only
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			rows, err := reviewRows(ctx, rt, account)
			if err != nil {
				return exitErr("%v", err)
			}
			o := outFor(cmd)
			if flags.json {
				return o.printJSON(rows)
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				table = append(table, []string{
					strconv.FormatInt(r.ID, 10), r.AccountID, r.Kind,
					clip(r.Reason, 60), r.CreatedAt.Format("01-02 15:04"),
				})
			}
			o.printTable([]string{"ID", "ACCOUNT", "KIND", "REASON", "CREATED"}, table)
			if len(rows) == 0 {
				o.println("Открытых ревью нет.")
			}
			return nil
		},
	}
}

func newReviewShowCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Show one open review item in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			rows, err := reviewRows(ctx, rt, account)
			if err != nil {
				return exitErr("%v", err)
			}
			var found *store.ReviewItem
			for i := range rows {
				if strconv.FormatInt(rows[i].ID, 10) == args[0] {
					found = &rows[i]
					break
				}
			}
			if found == nil {
				return exitErr("открытое ревью %s не найдено", args[0])
			}
			o := outFor(cmd)
			o.printf("ID: %d  аккаунт: %s  вид: %s  статус: %s\n", found.ID, found.AccountID, found.Kind, found.Status)
			o.println("Причина:", found.Reason)
			buf := &strings.Builder{}
			if err := indentJSON(found.Payload, buf); err == nil {
				o.println("Payload:")
				o.println(buf.String())
			} else {
				o.println("Payload:", found.Payload)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	return cmd
}

func newReviewResolveCmd() *cobra.Command {
	var (
		account string
		note    string
	)
	cmd := &cobra.Command{
		Use:   "resolve ID",
		Short: "Close a review item as resolved",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return exitErr("ID должен быть числом")
			}
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			if note == "" {
				note = "resolved via cli"
			}
			if err := review.New(rt.Webhooks, rt.Log).Resolve(ctx, rt.DB, id, note); err != nil {
				return exitErr("%v", err)
			}
			outFor(cmd).printf("Ревью %d закрыто.\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "resolution note")
	return cmd
}

func newReviewIgnoreCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "ignore ID",
		Short: "Close a review item as deliberately ignored",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return exitErr("ID должен быть числом")
			}
			opts := buildOpts()
			opts.Account = account
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown
			if err := review.New(rt.Webhooks, rt.Log).Ignore(ctx, rt.DB, id); err != nil {
				return exitErr("%v", err)
			}
			outFor(cmd).printf("Ревью %d проигнорировано.\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	return cmd
}

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "Candidate profile.md"}
	cmd.AddCommand(newProfileEditCmd())
	return cmd
}

func newProfileEditCmd() *cobra.Command {
	var account string
	return &cobra.Command{
		Use:   "edit",
		Short: "Open profile.md in $EDITOR (VISUAL/EDITOR, fallback vi)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("%v", err)
			}
			path := profilePathFor(cfg, account)
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return exitErr("dir: %v", err)
			}
			if _, err := os.Stat(path); os.IsNotExist(err) {
				head := renderProfileMD(profile.SuggestOnboardingQuestions(), nil)
				if werr := os.WriteFile(path, []byte(head), 0o600); werr != nil {
					return exitErr("create profile.md: %v", werr)
				}
			}
			editor := os.Getenv("VISUAL")
			if editor == "" {
				editor = os.Getenv("EDITOR")
			}
			if editor == "" {
				editor = "vi"
			}
			c := exec.Command(editor, path)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return exitErr("editor %q: %v", editor, err)
			}
			outFor(cmd).println("profile.md сохранён:", path)
			return nil
		},
	}
}
