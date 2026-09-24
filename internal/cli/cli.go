package cli

// Root command and the global flag set. Command implementations live in
// cmd_*.go; each command builds exactly the runtime slice it needs.

import (
	"fmt"

	"github.com/spf13/cobra"

	"hhauto/internal/config"
)

// Injected at release time via -ldflags (cmd/hhauto).
var (
	buildVersion = "dev"
	buildCommit  = "none"
)

type globalFlags struct {
	configPath string
	dryRun     bool
	logLevel   string
	json       bool
	noColor    bool
}

var flags globalFlags

func defaultConfigPath() string {
	return config.ExpandPath("~/.config/hhauto/hhauto.toml")
}

// Execute runs the CLI and returns the process exit code.
func Execute(version, commit string) int {
	buildVersion, buildCommit = version, commit
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "hhauto",
		Short:         "hh.ru automation: apply → funnel → interview (docs/PLAN.md)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&flags.configPath, "config", defaultConfigPath(), "path to hhauto.toml")
	pf.BoolVar(&flags.dryRun, "dry-run", false, "force dry-run: log every mutation, send nothing (overrides general.dry_run)")
	pf.StringVar(&flags.logLevel, "log-level", "", "override log.level (debug|info|warn|error)")
	pf.BoolVar(&flags.json, "json", false, "machine-readable JSON output where supported")
	pf.BoolVar(&flags.noColor, "no-color", false, "disable colored output (output is plain text today; accepted for scripts)")

	root.AddCommand(
		newOnboardingCmd(),
		newAccountCmd(),
		newRunCmd(),
		newStatusCmd(),
		newFunnelCmd(),
		newVacanciesCmd(),
		newResumeCmd(),
		newChatsCmd(),
		newContactsCmd(),
		newTestsCmd(),
		newReviewCmd(),
		newProfileCmd(),
		newAICmd(),
		newConfigCmd(),
		newDoctorCmd(),
		newBackupCmd(),
		newVersionCmd(),
	)
	return root
}

// buildOpts assembles per-command BuildOptions from the global flags.
func buildOpts() BuildOptions {
	return BuildOptions{
		ConfigPath: flags.configPath,
		DryRun:     flags.dryRun,
		LogLevel:   flags.logLevel,
	}
}

// outFor returns the output streams for a command context.
func outFor(cmd *cobra.Command) out {
	return out{stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
}

// exitErr formats a command failure for the cobra error path.
func exitErr(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
