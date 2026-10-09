package autoclone

// T35: the improve pipeline over ANY resume hash — the engine behind the
// standalone `hhauto resume improve` command. It reuses the T29
// plan→apply→verify machinery verbatim (per-field AI proposals, the
// browser editor apply or the builder-update API fallback, the было/стало
// artifact) with ONE difference: the clone-lineage invariant is NOT
// enforced here. Guarding base resumes is the COMMAND's job (--confirm);
// this method is only ever reachable through it or through tests.

import (
	"context"
	"fmt"
	"strings"
)

// ImproveAnyResume runs the improve flow for one arbitrary resume hash
// (base resume or tracked clone — the caller owns the base-resume
// confirm gate). Semantics mirror ImproveClone: per-field failures
// degrade to statuses on the artifact; only read/apply-surface failures
// return an error. The diff artifact persists on the lineage row when the
// hash happens to be a tracked clone; for base resumes it is only
// returned to the caller.
func (im *Improver) ImproveAnyResume(ctx context.Context, accountID, hash string) (*DiffArtifact, error) {
	// T44: every pipeline step logs — the standalone command used to sit
	// silent for the whole multi-minute per-field AI plan (GLM reasoning
	// at max runs ~1-2min per field), which read as a dead process.
	im.opt.Log.Info("autoclone: improve any-resume start",
		"account", accountID, "hash", hash, "dry_run", im.opt.DryRun,
		"fields", strings.Join(im.opt.Cfg.Fields, ","))
	live, err := im.opt.API.ResumeContent(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("autoclone: live content: %w", err)
	}
	im.opt.Log.Info("autoclone: improve live content read", "hash", hash)

	// T93 lineage-awareness: an untracked hash with a shard lineage edge
	// (parentResumeId in the stored snapshot) auto-registers; a tracked
	// clone improves with the full parent context either way.
	parentHash := im.ensureLineage(ctx, accountID, hash)
	parentCtx := im.parentContextOf(ctx, accountID, parentHash)

	artifact := &DiffArtifact{
		Schema:     artifactSchemaV1,
		CloneHash:  hash,
		ParentHash: parentHash,
		CreatedAt:  im.opt.Now().UTC(),
		DryRun:     im.opt.DryRun,
	}
	mutated := live.Clone()
	pendings := im.planFields(ctx, accountID, hash, live, mutated, artifact, parentHash, parentCtx)
	if im.opt.DryRun || len(pendings) == 0 {
		im.opt.Log.Info("autoclone: improve any-resume done (no apply)",
			"hash", hash, "dry_run", im.opt.DryRun, "planned_fields", len(artifact.Fields), "pending", len(pendings))
		im.persistIfTracked(ctx, hash, artifact)
		return artifact, nil
	}

	if im.opt.Editor != nil {
		if aerr := im.applyPendingViaEditor(ctx, accountID, hash, live, pendings, artifact); aerr != nil {
			return nil, aerr
		}
	} else {
		im.applyPending(ctx, hash, live, mutated, pendings, artifact)
	}
	artifact.Applied = artifactHasStatus(artifact, StatusImproved)
	im.opt.Log.Info("autoclone: improve any-resume done",
		"hash", hash, "planned_fields", len(artifact.Fields), "pending", len(pendings), "applied", artifact.Applied)
	im.persistIfTracked(ctx, hash, artifact)
	return artifact, nil
}

// persistIfTracked stores the artifact on the lineage row only when the
// hash is a tracked clone (best effort, like persist). A base resume has
// no row: skip the write instead of spamming the log with not-tracked
// errors.
func (im *Improver) persistIfTracked(ctx context.Context, hash string, artifact *DiffArtifact) {
	row, err := im.opt.DB.ResumeClonesGet(ctx, hash)
	if err != nil {
		im.opt.Log.Warn("autoclone: lineage lookup for improve diff", "hash", hash, "err", err)
		return
	}
	if row == nil {
		return
	}
	im.persist(ctx, hash, artifact)
}
