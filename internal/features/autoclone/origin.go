// The clone_original_action policy (T29): after a successful full clone
// (+improve) the source resume may be kept (default), hidden (visibility
// no_one — reversible) or deleted. Every unknown or failed action degrades
// to keep with a WARN: the source resume is never lost to an automation
// bug (fail-safe).

package autoclone

import (
	"context"
	"log/slog"
)

// OriginMutator is the management surface the origin action needs (the
// hhapi client implements it; tests substitute fakes).
type OriginMutator interface {
	HideResume(ctx context.Context, hash string) error
	ShowResume(ctx context.Context, hash string) error
	DeleteResume(ctx context.Context, hash string) error
}

// validOriginAction reports whether action is in the vocabulary.
func validOriginAction(action string) bool {
	switch action {
	case "keep", "hide", "delete":
		return true
	default:
		return false
	}
}

// ApplyOriginAction executes the configured source-resume policy. The
// caller invokes it only after a verified clone exists; this function
// never inspects the clone — the guard lives at the call site.
func ApplyOriginAction(ctx context.Context, api OriginMutator, action, originalHash string, log *slog.Logger) {
	switch action {
	case "keep", "":
		return
	case "hide":
		if err := api.HideResume(ctx, originalHash); err != nil {
			log.Warn("autoclone: clone_original_action=hide failed; keeping the original (fail-safe)",
				"resume", originalHash, "err", err)
		}
	case "delete":
		if err := api.DeleteResume(ctx, originalHash); err != nil {
			log.Warn("autoclone: clone_original_action=delete failed; keeping the original (fail-safe)",
				"resume", originalHash, "err", err)
		}
	default:
		log.Warn("autoclone: unknown clone_original_action; keeping the original (fail-safe)",
			"action", action, "resume", originalHash)
	}
}
