package autoclone

// T93 lineage-awareness: clone lineage edges are resolved from the hh
// shard items (each item's _attributes carries the resume's numeric id and
// its parent's numeric id) and registered through the store's idempotent
// ResumeClonesRegister. A tracked lineage unlocks the parent context for
// every improve task: the family positioning (base title) and — when it
// differs — the same field's parent value.

import (
	"context"
	"encoding/json"
	"strings"

	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// LineageEdge is one resolved clone→parent pair (hashes, not numeric ids).
type LineageEdge struct {
	Clone  string
	Parent string
}

// LineageEdges resolves the clone→parent edges from shard resume items.
// The numeric→hash mapping comes from _attributes.id; items without a
// usable hash/parent pair (and self-parenting fixtures) are skipped.
func LineageEdges(resumes []hhapi.Resume) []LineageEdge {
	hashByID := make(map[string]string, len(resumes))
	hashes := make([]string, len(resumes))
	parents := make([]string, len(resumes))
	for i, r := range resumes {
		var item struct {
			Attrs struct {
				ID             json.RawMessage `json:"id"`
				Hash           string          `json:"hash"`
				ParentResumeID json.RawMessage `json:"parentResumeId"`
			} `json:"_attributes"`
		}
		if err := json.Unmarshal(r.Raw, &item); err != nil {
			item.Attrs.Hash = r.HHID
		}
		hash := item.Attrs.Hash
		if hash == "" {
			hash = r.HHID
		}
		hashes[i] = hash
		parents[i] = shardScalar(item.Attrs.ParentResumeID)
		if id := shardScalar(item.Attrs.ID); id != "" && hash != "" {
			hashByID[id] = hash
		}
	}
	var edges []LineageEdge
	seen := make(map[string]bool, len(resumes))
	for i, parentID := range parents {
		clone, parent := hashes[i], hashByID[parentID]
		if clone == "" || parent == "" || parent == clone || seen[clone] {
			continue
		}
		seen[clone] = true
		edges = append(edges, LineageEdge{Clone: clone, Parent: parent})
	}
	return edges
}

// shardScalar renders a raw JSON scalar (string or number) as a string.
func shardScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// ensureLineage returns the parent hash of the resume: a tracked lineage
// row wins; otherwise the account's stored snapshots are searched for a
// shard lineage edge, which is then registered (idempotent upsert —
// ImproveAnyResume auto-registers clones created outside the engine).
// "" = no known lineage (a base resume or nothing resolvable).
func (im *Improver) ensureLineage(ctx context.Context, accountID, hash string) string {
	row, err := im.opt.DB.ResumeClonesGet(ctx, hash)
	if err != nil {
		im.opt.Log.Warn("autoclone: lineage lookup failed", "hash", hash, "err", err)
		return ""
	}
	if row != nil {
		return row.ParentHash
	}
	rows, err := im.opt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		im.opt.Log.Warn("autoclone: resume snapshots unavailable for lineage resolution", "account", accountID, "err", err)
		return ""
	}
	items := make([]hhapi.Resume, 0, len(rows))
	for _, r := range rows {
		items = append(items, hhapi.Resume{HHID: r.HhResumeID, Raw: json.RawMessage(r.Raw)})
	}
	for _, e := range LineageEdges(items) {
		if e.Clone != hash {
			continue
		}
		if err := im.opt.DB.ResumeClonesRegister(ctx, &store.ResumeClone{
			CloneHash: e.Clone, ParentHash: e.Parent,
			Depth: im.parentDepth(ctx, e.Parent) + 1, AccountID: accountID,
		}); err != nil {
			im.opt.Log.Warn("autoclone: lineage auto-registration failed", "clone", e.Clone, "err", err)
			return ""
		}
		im.opt.Log.Info("autoclone: clone lineage auto-registered", "clone", e.Clone, "parent", e.Parent)
		return e.Parent
	}
	return ""
}

// parentDepth reads the lineage depth of a parent hash (0 when untracked).
func (im *Improver) parentDepth(ctx context.Context, parentHash string) int {
	row, err := im.opt.DB.ResumeClonesGet(ctx, parentHash)
	if err != nil || row == nil {
		return 0
	}
	return row.Depth
}

// parentContext is the run-scoped lineage anchor: the base resume's live
// content with its title and the role families its title/key skills name.
type parentContext struct {
	hash    string
	title   string
	content *hhapi.ResumeContent
}

// parentContextOf fetches the parent's live content once per improve run.
// A missing reader or a fetch failure degrades to nil — the run continues
// without lineage facts and WITHOUT a family anchor (never anchor a clone
// on its own possibly-drifted title).
func (im *Improver) parentContextOf(ctx context.Context, accountID, parentHash string) *parentContext {
	if parentHash == "" {
		return nil
	}
	if im.opt.ResumeContentFor == nil {
		im.opt.Log.Warn("autoclone: no content reader wired; improving without the parent context", "parent", parentHash)
		return nil
	}
	content, err := im.opt.ResumeContentFor(ctx, accountID, parentHash)
	if err != nil {
		im.opt.Log.Warn("autoclone: parent content read failed; improving without the parent context",
			"parent", parentHash, "err", err)
		return nil
	}
	return &parentContext{hash: parentHash, title: contentTitle(content), content: content}
}

// parentFieldValue returns the parent's current value of the same field
// target (matched by locator — same payload key and item index).
func parentFieldValue(spec FieldSpec, parent *hhapi.ResumeContent, tg target) (json.RawMessage, bool) {
	for _, t := range expandTargets(spec, parent) {
		if t.locator == tg.locator {
			return t.current, true
		}
	}
	return nil, false
}

// contentTitle extracts the resume title string from the builder state.
func contentTitle(content *hhapi.ResumeContent) string {
	return conditionString(content.Resume["title"])
}

// contentKeySkills joins the resume's key skill names (family tokens ride
// the base title AND its key skills).
func contentKeySkills(content *hhapi.ResumeContent) string {
	return strings.Join(conditionStrings(content.Resume["keySkills"]), " ")
}

// conditionString unwraps the hh condition format [{"string": v}] to the
// first string (empty when the value is not in that shape).
func conditionString(raw json.RawMessage) string {
	parts := conditionStrings(raw)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// conditionStrings unwraps every string of the condition format.
func conditionStrings(raw json.RawMessage) []string {
	var parts []struct {
		String string `json:"string"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.String != "" {
			out = append(out, p.String)
		}
	}
	return out
}
