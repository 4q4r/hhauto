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
	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/features/autoclone"
	"hhauto/internal/hhapi"
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
func injectImproveTask(cfg *config.Config) {
	if !cfg.Features.AutoClone.Improve.Enabled {
		return
	}
	if cfg.AI.Tasks == nil {
		cfg.AI.Tasks = map[string]config.AITask{}
	}
	if _, ok := cfg.AI.Tasks["resume_improve"]; !ok {
		cfg.AI.Tasks["resume_improve"] = config.AITask{
			ModelProfile: cfg.Features.AutoClone.Improve.ModelProfile,
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

// buildImprove constructs the clone improver from the improve config.
// Returns (nil, nil) when improve is disabled. Dry-run rides the general
// config flag: the diff is computed and logged, nothing is applied.
// apiFor resolves the per-account content surface; ckStore feeds the
// browser editor runner (required when editor.enabled).
func buildImprove(cfg *config.Config, runner ai.Runner, db interface {
	ResumeClonesGet(ctx context.Context, cloneHash string) (*store.ResumeClone, error)
	ResumeClonesSetImproveDiff(ctx context.Context, cloneHash, diffJSON string) error
}, apiFor func(accountID string) (contentEditorAPI, error), ckStore cookies.Store, log *slog.Logger) (*autoclone.Improver, error) {
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
	var browser autoclone.EditorRunner
	if improve.Editor.Enabled {
		if ckStore == nil {
			return nil, fmt.Errorf("autoclone: improve.editor.enabled requires the cookie store (the browser needs the account session)")
		}
		browser, err = autoclone.NewChromeEditorRunner(autoclone.EditorOptions{
			Cfg:     improve.Editor,
			DB:      storeDB,
			Cookies: editorCookieSource{store: ckStore},
			Verify:  editor,
			Log:     log,
		})
		if err != nil {
			return nil, err
		}
	}
	return autoclone.New(autoclone.Options{
		API:      editor,
		Editor:   browser,
		Runner:   fpr,
		DB:       storeDB,
		Registry: registry,
		Cfg:      improve,
		DryRun:   cfg.General.DryRun,
		Log:      log,
	})
}
