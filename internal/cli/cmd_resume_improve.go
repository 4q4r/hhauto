package cli

// T35: `hhauto resume improve` — the standalone resume-improvement command.
// It runs the T29 improve pipeline (per-field AI proposals → apply through
// the browser editor or the builder-update API → было/стало artifact) over
// ANY resume, not only clones:
//
//	hhauto resume improve --resume HASH [--account ID] [--fields title,about]
//	                      [--dry-run] [--confirm]
//
// Invariants:
//   - a BASE (non-clone) resume is only touched with --confirm — the
//     safe-by-default gate for the owner's real resume; clones improve
//     without it;
//   - the command takes the resume_improve task lock, so it never races a
//     second improver; a legacy `hhauto run` (task type all) also holds it;
//   - the diff is always printed (was/stale per field); --dry-run stops
//     before any apply.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"hhauto/internal/features/autoclone"
	"hhauto/internal/tasklock"
)

func newResumeImproveCmd() *cobra.Command {
	var (
		hash    string
		account string
		fields  string
		dryRun  bool
		confirm bool
	)
	cmd := &cobra.Command{
		Use:   "improve",
		Short: "Improve one resume's fields through the AI improve pipeline (base resumes need --confirm)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(hash) == "" {
				return exitErr("укажите --resume HASH")
			}
			opts := buildOpts()
			opts.Account = account
			opts.ResumeImprove = true // force the resume_improve AI task injection
			ctx := cmd.Context()
			rt, err := Build(ctx, opts)
			if err != nil {
				return exitErr("wiring: %v", err)
			}
			defer func() { _ = rt.Close() }() // command teardown

			// Multi-instance lock: the improve work claims the
			// resume_improve type (coexists with a --task-type multiapply
			// engine; conflicts with another improver and with `all`).
			unlock, err := acquireTaskLock(rt.Cfg.General.DataDir, "resume_improve")
			if err != nil {
				return exitErr("%v", err)
			}
			defer unlock()

			o := outFor(cmd)
			return runResumeImprove(ctx, rt, o, improveRequest{
				Hash: hash, Account: account, Fields: fields, DryRun: dryRun, Confirm: confirm,
			})
		},
	}
	cmd.Flags().StringVar(&hash, "resume", "", "resume hash to improve (required)")
	cmd.Flags().StringVar(&account, "account", "", "account id (resolved from the clone lineage or stored snapshots when omitted)")
	cmd.Flags().StringVar(&fields, "fields", "", "comma-separated field subset to improve (default: all improve.fields from config)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the было/стало diff without applying")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "allow improving a BASE (non-clone) resume — the owner's real resume")
	return cmd
}

// improveRequest is the parsed command input.
type improveRequest struct {
	Hash    string
	Account string
	Fields  string
	DryRun  bool
	Confirm bool
}

// runResumeImprove resolves the target, enforces the base-resume confirm
// gate, runs the pipeline and renders the diff.
func runResumeImprove(ctx context.Context, rt *Runtime, o out, req improveRequest) error {
	accountID, isClone, err := resolveImproveTarget(ctx, rt, req.Hash, req.Account)
	if err != nil {
		return exitErr("%v", err)
	}
	if !isClone && !req.Confirm {
		return exitErr("резюме %s — базовое (не клон): улучшение боевого резюме требует явного --confirm", req.Hash)
	}
	if rt.AI == nil {
		return exitErr("AI-раннер не сконфигурирован: задайте [features.autoclone.improve] model_profile (или [ai.tasks.resume_improve])")
	}

	improve := rt.Cfg.Features.AutoClone.Improve
	if req.Fields != "" {
		chosen, err := parseFieldsFlag(req.Fields, improve.Fields)
		if err != nil {
			return exitErr("%v", err)
		}
		improve.Fields = chosen
	}
	improve.Enabled = true // the command IS the enable signal (one-shot intent)

	// T44: the start is announced immediately via the console log handler —
	// the per-field AI plan takes minutes at GLM reasoning max, and a fully
	// silent stretch read as a dead process. T52: a single line (no manual
	// stdout duplicate of the same event).
	rt.Log.Info("resume improve: start",
		"hash", req.Hash, "account", accountID, "clone", isClone,
		"fields", improve.Fields, "dry_run", req.DryRun)

	improver, err := buildImproveForCommand(rt, improve, accountID, req.DryRun)
	if err != nil {
		return exitErr("%v", err)
	}

	var artifact *autoclone.DiffArtifact
	if isClone {
		row, err := rt.DB.ResumeClonesGet(ctx, req.Hash)
		if err != nil || row == nil {
			return exitErr("lineage: %v", err)
		}
		artifact, err = improver.ImproveClone(ctx, accountID, row.ParentHash, req.Hash)
		if err != nil {
			return exitErr("improve: %v", err)
		}
	} else {
		artifact, err = improver.ImproveAnyResume(ctx, accountID, req.Hash)
		if err != nil {
			return exitErr("improve: %v", err)
		}
	}
	return printImproveDiff(o, artifact, req.DryRun)
}

// accountFlagOf keeps the --account flag in the request flow: the runtime
// was already account-filtered by Build, so the flag re-enters as the
// filter value.

// resolveImproveTarget finds the owning account and clone status for a
// resume hash: --account wins (validated against the lineage), then the
// clone lineage, then the stored resume snapshots.
func resolveImproveTarget(ctx context.Context, rt *Runtime, hash, accountFlag string) (string, bool, error) {
	row, err := rt.DB.ResumeClonesGet(ctx, hash)
	if err != nil {
		return "", false, fmt.Errorf("lineage lookup: %w", err)
	}
	if accountFlag != "" {
		if row != nil && row.AccountID != accountFlag {
			return "", false, fmt.Errorf("резюме %s принадлежит аккаунту %q, а не %q", hash, row.AccountID, accountFlag)
		}
		if _, ok := rt.Cfg.Accounts[accountFlag]; !ok {
			return "", false, fmt.Errorf("аккаунт %q не сконфигурирован", accountFlag)
		}
		return accountFlag, row != nil, nil
	}
	if row != nil {
		return row.AccountID, true, nil
	}
	ids := make([]string, 0, len(rt.Cfg.Accounts))
	for id := range rt.Cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		items, err := rt.DB.ResumesListByAccount(ctx, id)
		if err != nil {
			return "", false, fmt.Errorf("resumes %s: %w", id, err)
		}
		for _, r := range items {
			if r.HhResumeID == hash {
				return id, false, nil
			}
		}
	}
	return "", false, fmt.Errorf("резюме %s не найдено (ни в клонах, ни в снапшотах): уточните --account", hash)
}

// parseFieldsFlag turns --fields "title, about" into a validated field id
// list; empty input selects the config defaults.
func parseFieldsFlag(raw string, defaults []string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), defaults...), nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, tok := range strings.Split(raw, ",") {
		id := strings.TrimSpace(tok)
		if id == "" {
			return nil, fmt.Errorf("--fields: пустой элемент в %q", raw)
		}
		if _, ok := autoclone.Spec(id); !ok {
			return nil, fmt.Errorf("--fields: неизвестное поле %q", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// printImproveDiff renders the было/стало artifact (--json or, on a
// terminal, one compact block per field; the plain table otherwise).
func printImproveDiff(o out, artifact *autoclone.DiffArtifact, _ bool) error {
	if flags.json {
		return o.printJSON(artifact)
	}
	if o.st.enabled {
		printImproveBlocks(o, artifact)
		return nil
	}
	head := "improve diff"
	if artifact.DryRun {
		head += " (dry-run: ничего не применялось)"
	} else if artifact.Applied {
		head += " (применено)"
	} else {
		head += " (применения не было)"
	}
	o.println(head)
	rows := make([][]string, 0, len(artifact.Fields))
	for _, f := range artifact.Fields {
		reason := f.Reason
		rows = append(rows, []string{
			f.Field, f.Status,
			clip(string(f.Before), 500), clip(string(f.After), 500), clip(reason, 200),
		})
	}
	o.printTable([]string{"FIELD", "STATUS", "БЫЛО", "СТАЛО", "ПРИЧИНА"}, rows)
	return nil
}

// diff block layout: two-space indent, labels padded to the widest one
// ("причина:", 8 runes) plus one space → the value column starts at 11.
const (
	diffLabelWidth = 8
	diffValueCol   = 11
)

// printImproveBlocks renders the artifact as one compact block per field:
// verdict first (colored glyph + status), field name after, the
// было/стало values wrapped to the terminal width, the reason on its own
// line. Same data as the plain table, shaped for reading.
func printImproveBlocks(o out, artifact *autoclone.DiffArtifact) {
	st := o.st
	head := "improve diff"
	switch {
	case artifact.DryRun:
		head += " " + st.cyan("(dry-run: ничего не применялось)")
	case artifact.Applied:
		head += " " + st.green("(применено)")
	default:
		head += " " + st.dim("(применения не было)")
	}
	o.println(head)
	width := ttyWidth(o.stdout) - diffValueCol
	for _, f := range artifact.Fields {
		o.println()
		o.println(st.status(f.Status) + "  " + f.Field)
		printDiffValue(o, "было:", string(f.Before), width)
		printDiffValue(o, "стало:", string(f.After), width)
		printDiffValue(o, "причина:", f.Reason, width)
	}
}

// printDiffValue prints one labeled value block of a diff field, wrapping
// to width; empty values print nothing.
func printDiffValue(o out, label, value string, width int) {
	if value == "" {
		return
	}
	for i, line := range strings.Split(wrapText(value, width), "\n") {
		if i == 0 {
			o.println("  " + padCell(o.st.dim(label), diffLabelWidth) + " " + line)
			continue
		}
		o.println("  " + strings.Repeat(" ", diffLabelWidth) + " " + line)
	}
}

// errUnknownTaskType marks a --task-type value outside the vocabulary.
var errUnknownTaskType = errors.New("unknown task type")

// normalizeTaskType validates the --task-type value ("" = all).
func normalizeTaskType(raw string) (string, error) {
	if raw == "" {
		return tasklock.TypeAll, nil
	}
	if err := tasklock.Validate(raw); err != nil {
		return "", fmt.Errorf("%w: %v", errUnknownTaskType, err)
	}
	return raw, nil
}

// runTaskTypeFeature maps a run task type onto the feature filter applied
// to the config copy. ok=false marks types with no recurring engine
// workers: resume_improve is command-scoped work (`hhauto resume improve`),
// not an engine feature.
func runTaskTypeFeature(taskType string) (feature string, ok bool) {
	switch taskType {
	case tasklock.TypeAll:
		return "", true
	case tasklock.TypeMultiApply:
		return "multiapply", true
	default:
		return "", false
	}
}
