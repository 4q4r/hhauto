package cli

// hhauto rejections analyze (T123): the rejection loop MVP. Collects the
// discard-filtered negotiations, analyzes the fresh ones with the
// reject_analysis AI task and prints the category report + fix-plan.
// MVP policy: REPORT-ONLY — the owner decides on every change; nothing is
// auto-applied to the resume.

import (
	"fmt"

	"github.com/spf13/cobra"

	"hhauto/internal/features/rejections"
)

func newRejectionsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "rejections", Short: "Rejection loop (analyze)"}
	cmd.AddCommand(newRejectionsAnalyzeCmd())
	return cmd
}

func newRejectionsAnalyzeCmd() *cobra.Command {
	var (
		account string
		days    int
	)
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Разобрать свежие отказы: сбор → ИИ-анализ → отчёт (авто-правок нет)",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			if _, ok := rt.Cfg.AI.Tasks["reject_analysis"]; !ok {
				return exitErr("ИИ-задача reject_analysis не настроена: добавьте в конфиг секцию [ai.tasks.reject_analysis] с model_profile")
			}
			api, err := rt.APIFor(id)
			if err != nil {
				return exitErr("api %s: %v", id, err)
			}
			prof, err := rt.profileFor(ctx, id)
			if err != nil {
				return exitErr("profile %s: %v", id, err)
			}
			res, err := rejections.Analyze(ctx, rejections.Options{
				AccountID: id,
				API:       api,
				Runner:    rt.AI,
				DB:        rt.DB,
				Profile:   prof,
				Out:       cmd.OutOrStdout(),
				DataDir:   rt.Cfg.General.DataDir,
				Days:      days,
			})
			if err != nil {
				return exitErr("rejections: %v", err)
			}
			if res.ReportPath != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Отчёт: %s\n", res.ReportPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "restrict to one account")
	cmd.Flags().IntVar(&days, "days", 30, "окно анализа, дней")
	return cmd
}
