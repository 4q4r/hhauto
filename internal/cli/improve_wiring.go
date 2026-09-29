// T29 improve wiring: the [features.autoclone.improve] section rides the
// shared AI gateway. The model profile is declared once in the improve
// section; the CLI injects the matching [ai.tasks.resume_improve] slot
// (an explicit owner task entry wins) and constructs the improver with
// fail-fast validation of the field vocabulary.
//
// T30 adds the browser editor runner: when
// [features.autoclone.improve.editor] is enabled, the planned values are
// applied through the real editor pages (chromedp; the /profile/** POST
// family is edge-gated for off-browser TLS). The runner's per-account
// session comes from the deployed cookie store, and its read-back verify
// rides the same per-account content surface as the improver itself.

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"hhauto/internal/ai"
	"hhauto/internal/aiprompts"

	// Aliased: the wiring below declares a local `browser` variable.
	sysbrowser "hhauto/internal/browser"
	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/features/autoclone"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// contentEditorAPI is the per-account surface the improve pipeline and
// the editor runner both need (type-asserted off the full HHAPI client).
type contentEditorAPI interface {
	hhapi.ResumeContentEditor
}

// injectImproveTask ensures the resume_improve task exists when improve is
// enabled: the model profile always comes from the improve section (single
// source of truth); an explicit [ai.tasks.resume_improve] entry wins.
// force=true (T35, the `resume improve` command) injects even when the
// improve feature itself is disabled — the command is the enable signal;
// an empty model profile is left to the command's fail-fast error.
func injectImproveTask(cfg *config.Config, force bool) {
	improve := cfg.Features.AutoClone.Improve
	if !improve.Enabled && !force {
		return
	}
	if improve.ModelProfile == "" {
		return
	}
	if cfg.AI.Tasks == nil {
		cfg.AI.Tasks = map[string]config.AITask{}
	}
	if _, ok := cfg.AI.Tasks["resume_improve"]; !ok {
		cfg.AI.Tasks["resume_improve"] = config.AITask{
			ModelProfile: improve.ModelProfile,
		}
	}
}

// lineageEditor resolves the per-account content editor for a clone hash.
// hhapi clients are per-account (each owns its cookie jar and transport),
// while the improve/editor surfaces are addressed by clone hash — the
// local lineage maps between them and keeps the base-resume invariant at
// the wiring layer (an untracked hash never reaches a client).
type lineageEditor struct {
	db     *store.DB
	apiFor func(accountID string) (contentEditorAPI, error)
}

func (l *lineageEditor) editorFor(ctx context.Context, hash string) (contentEditorAPI, error) {
	row, err := l.db.ResumeClonesGet(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("cli: lineage lookup for %q: %w", hash, err)
	}
	if row == nil {
		return nil, fmt.Errorf("cli: hash %q is not a tracked clone", hash)
	}
	ed, err := l.apiFor(row.AccountID)
	if err != nil {
		return nil, fmt.Errorf("cli: content editor for account %q: %w", row.AccountID, err)
	}
	return ed, nil
}

// ResumeContent reads the mutable split of one clone through its account.
func (l *lineageEditor) ResumeContent(ctx context.Context, hash string) (*hhapi.ResumeContent, error) {
	ed, err := l.editorFor(ctx, hash)
	if err != nil {
		return nil, err
	}
	return ed.ResumeContent(ctx, hash)
}

// UpdateResumeContent posts a modified split through the clone's account.
func (l *lineageEditor) UpdateResumeContent(ctx context.Context, hash string, content *hhapi.ResumeContent, screens []string) error {
	ed, err := l.editorFor(ctx, hash)
	if err != nil {
		return err
	}
	return ed.UpdateResumeContent(ctx, hash, content, screens)
}

// editorCookieSource adapts the deployed cookie store onto the runner's
// EditorCookieSource. Decryption stays inside the store implementation;
// the session only ever reaches the runner's isolated browser profile.
type editorCookieSource struct {
	store cookies.Store
}

// EditorCookies implements autoclone.EditorCookieSource.
func (e editorCookieSource) EditorCookies(ctx context.Context, accountID string) ([]*http.Cookie, error) {
	return e.store.Load(ctx, accountID)
}

// buildEditorRunner constructs the T30 browser editor runner when
// [features.autoclone.improve.editor] is enabled. accountID pins the
// owning account for the plan (T35: the resume-improve command drives
// BASE resumes — pinned skips the lineage resolution); "" keeps the strict
// clone-lineage account lookup. verify is the read-back surface. Returns
// (nil, nil) when the editor is disabled (the builder-update API path is
// the apply surface).
func buildEditorRunner(cfg *config.Config, db *store.DB, ckStore cookies.Store,
	verify autoclone.ContentReader, accountID string, log *slog.Logger,
) (autoclone.EditorRunner, error) {
	improve := cfg.Features.AutoClone.Improve
	if !improve.Editor.Enabled {
		return nil, nil
	}
	if ckStore == nil {
		return nil, fmt.Errorf("autoclone: improve.editor.enabled requires the cookie store (the browser needs the account session)")
	}
	return autoclone.NewChromeEditorRunner(autoclone.EditorOptions{
		Cfg:            improve.Editor,
		DB:             db,
		Cookies:        editorCookieSource{store: ckStore},
		Verify:         verify,
		AccountID:      accountID,
		ChromeProvider: sysbrowser.NewFromConfig(cfg.Browser, log),
		BrowserCfg:     cfg.Browser,
		Log:            log,
	})
}

// improveBasePrompt resolves the resume_improve task's base system prompt
// (builtin v5 or the [ai.tasks.resume_improve] TOML override) through the
// same registry the AI runner uses — the per-field prompts compose on top
// of it (the RunWithSystem contract).
func improveBasePrompt(cfg *config.Config) (string, error) {
	reg, err := ai.NewPromptRegistry(cfg.AI.Tasks, aiprompts.FS)
	if err != nil {
		return "", err
	}
	body, _, err := reg.SystemPrompt(ai.TaskResumeImprove)
	return body, err
}

// buildImprove constructs the clone improver from the improve config.
// Returns (nil, nil) when improve is disabled. Dry-run rides the general
// config flag: the diff is computed and logged, nothing is applied.
// apiFor resolves the per-account content surface; ckStore feeds the
// browser editor runner (required when editor.enabled); profileFor loads
// the trusted candidate context (nil or a failure degrades to an empty
// context).
func buildImprove(cfg *config.Config, runner ai.Runner, db interface {
	ResumeClonesGet(ctx context.Context, cloneHash string) (*store.ResumeClone, error)
	ResumeClonesSetImproveDiff(ctx context.Context, cloneHash, diffJSON string) error
}, apiFor func(accountID string) (contentEditorAPI, error), ckStore cookies.Store, log *slog.Logger,
	profileFor func(ctx context.Context, accountID string) (profile.Profile, error)) (*autoclone.Improver, error) {
	improve := cfg.Features.AutoClone.Improve
	if !improve.Enabled {
		return nil, nil
	}
	fpr, ok := runner.(ai.FieldPromptRunner)
	if !ok {
		return nil, fmt.Errorf("autoclone: improve.enabled but the AI runner does not support per-field prompts (FieldPromptRunner)")
	}
	storeDB, ok := db.(*store.DB)
	if !ok {
		return nil, fmt.Errorf("autoclone: improve.enabled requires the store (lineage invariant)")
	}
	registry, err := autoclone.NewPromptRegistry(improve)
	if err != nil {
		return nil, err
	}
	editor := &lineageEditor{db: storeDB, apiFor: apiFor}
	browser, err := buildEditorRunner(cfg, storeDB, ckStore, editor, "", log)
	if err != nil {
		return nil, err
	}
	base, err := improveBasePrompt(cfg)
	if err != nil {
		return nil, err
	}
	return autoclone.New(autoclone.Options{
		API:        editor,
		Editor:     browser,
		Runner:     fpr,
		DB:         storeDB,
		Registry:   registry,
		Cfg:        improve,
		DryRun:     cfg.General.DryRun,
		Log:        log,
		ProfileFor: profileFor,
		BasePrompt: base,
	})
}

// buildNeedApplier constructs the T54 needs_human resolver for one review
// item: the item account's content surface plus the browser editor runner
// when [features.autoclone.improve.editor] is enabled. No AI wiring — the
// value was already proposed, the human approved (or corrected) it.
func buildNeedApplier(rt *Runtime, accountID string) (*autoclone.NeedApply, error) {
	editor, err := rt.contentEditorFor(accountID)
	if err != nil {
		return nil, err
	}
	browser, err := buildEditorRunner(rt.Cfg, rt.DB, rt.Cookies, editor, accountID, rt.Log)
	if err != nil {
		return nil, err
	}
	return &autoclone.NeedApply{API: editor, Editor: browser}, nil
}

// buildImproveForCommand constructs the improver for the standalone
// `resume improve` command (T35): the content surface is the ACCOUNT's own
// client (not the clone-lineage editor — base resumes have no lineage
// row), the editor runner pins the account, and the config copy carries
// the command's field/dry-run choices. cfg.Enabled is already forced by
// the caller (the command is the enable signal).
func buildImproveForCommand(rt *Runtime, improve config.AutoCloneImprove, accountID string, dryRun bool) (*autoclone.Improver, error) {
	fpr, ok := rt.AI.(ai.FieldPromptRunner)
	if !ok {
		return nil, fmt.Errorf("autoclone: the AI runner does not support per-field prompts (FieldPromptRunner)")
	}
	editor, err := rt.contentEditorFor(accountID)
	if err != nil {
		return nil, err
	}
	browser, err := buildEditorRunner(rt.Cfg, rt.DB, rt.Cookies, editor, accountID, rt.Log)
	if err != nil {
		return nil, err
	}
	registry, err := autoclone.NewPromptRegistry(improve)
	if err != nil {
		return nil, err
	}
	base, err := improveBasePrompt(rt.Cfg)
	if err != nil {
		return nil, err
	}
	return autoclone.New(autoclone.Options{
		API:        editor,
		Editor:     browser,
		Runner:     fpr,
		DB:         rt.DB,
		Registry:   registry,
		Cfg:        improve,
		DryRun:     dryRun || rt.Cfg.General.DryRun,
		Log:        rt.Log,
		ProfileFor: rt.profileFor,
		BasePrompt: base,
	})
}
