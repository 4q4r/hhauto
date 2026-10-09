package hhapi

// T99 created-resume experience dedup. Live mechanism (dumps 2026-10-09,
// /tmp/opencode/t99/): the account experience pool is ONE lineage pool —
// every record lists in "resumes" the resumes that render it. Publishing a
// created resume attaches its hash to EVERY pool record, so the fresh
// resume renders all four generations per employer (the owner-visible
// bug). hh has no detach endpoint: the editor delete button is a GLOBAL
// delete (T56 modal "исчезнет из всех резюме"), and the replace route
// ignores the resumes field. The only per-resume mechanism is the EDIT
// route's FORK (live T67): editing a shared record forks it for the
// editing resume and the original drops that resume's hash. Detach is
// therefore fork + delete-the-fork (the fork is a sole-member record, so
// the T56 delete machine applies): the original loses the resume, the
// fork dies, the pool keeps its twelve records — net zero.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// dedupForkBudget bounds the wait for one fork to materialize (async hh
// queue — the T67 lesson); dedupPollInterval is the poll step. Package
// variables so tests can shrink them.
var (
	dedupForkBudget   = 600 * time.Second
	dedupPollInterval = 10 * time.Second
)

// ErrExperienceForkTimeout reports a fork that never materialized within
// the budget. The state stays consistent (earlier detachments persist) and
// a re-run continues where the call stopped.
var ErrExperienceForkTimeout = errors.New("hhapi: experience fork did not materialize")

// ErrExperienceInPlace reports the unexpected live branch: hh answered the
// fork-provoking edit by changing the SHARED original in place. The record
// is reverted before the error returns; render impact is a single trailing
// newline until the revert lands.
var ErrExperienceInPlace = errors.New("hhapi: shared experience record edited in place")

// DetachExperienceResult reports one DetachDuplicateExperience run.
type DetachExperienceResult struct {
	// Kept holds one record id per employer — the newest generation left
	// rendering on the resume.
	Kept []int64
	// Detached holds the record ids that stopped rendering on the resume
	// (shared originals plus deleted crash-orphan forks).
	Detached []int64
	// Skipped holds lone-member records outside the dedup mechanism,
	// left untouched (the method never deletes content it cannot
	// attribute to this mechanism).
	Skipped []int64
}

// DetachDuplicateExperience prunes the duplicate experience generations a
// created resume inherited from the account pool: among the records that
// render on hash it keeps the newest (highest record id — end dates are
// identical within an employer) per company and detaches the rest. Records
// rendered only by hash are kept as-is unless they are crash orphans of a
// previous run (description equal to another rendered record's plus one
// trailing newline — the fork-provoking tweak), which are deleted
// directly. Shared records are detached through fork + delete-fork. The
// call is idempotent: with one generation per employer it performs no
// mutations.
func (c *Client) DetachDuplicateExperience(ctx context.Context, hash string) (DetachExperienceResult, error) {
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		return DetachExperienceResult{}, err
	}
	raw, ok := st.ProfileFields["experience"]
	if !ok {
		// No experience section in the builder state — nothing to prune
		// (the fresh-shell shape; the create→improve chain calls this
		// before the resume ever rendered a pool record).
		return DetachExperienceResult{Kept: []int64{}, Detached: []int64{}, Skipped: []int64{}}, nil
	}
	pool, err := experiencePoolRaw(raw)
	if err != nil {
		return DetachExperienceResult{}, err
	}

	baselineSHA := map[int64]string{}
	rendered := make([]experienceRecord, 0, len(pool))
	for _, rec := range pool {
		baselineSHA[rec.ID] = experienceDescSHA(rec.Raw)
		if rec.rendersOn(hash) {
			rendered = append(rendered, rec)
		}
	}

	res := DetachExperienceResult{Kept: []int64{}, Detached: []int64{}, Skipped: []int64{}}
	for _, group := range experienceGroups(rendered) {
		var shared, lone []experienceRecord
		for _, rec := range group {
			if len(rec.ResumeHashes) > 1 {
				shared = append(shared, rec)
			} else {
				lone = append(lone, rec)
			}
		}
		// Crash orphans of an interrupted run: a lone fork carrying the
		// tweak signature. Everything else lone stays (never delete
		// content this mechanism did not create).
		for _, rec := range lone {
			if experienceIsOrphan(rec, group) {
				if _, derr := c.DeleteExperienceRecords(ctx, hash, []int64{rec.ID}); derr != nil {
					return res, derr
				}
				res.Detached = append(res.Detached, rec.ID)
			} else {
				res.Skipped = append(res.Skipped, rec.ID)
			}
		}
		if len(shared) == 0 {
			continue
		}
		sort.Slice(shared, func(i, j int) bool { return shared[i].ID < shared[j].ID })
		keep := shared[len(shared)-1] // ascending sort → newest generation
		res.Kept = append(res.Kept, keep.ID)
		for _, rec := range shared[:len(shared)-1] {
			if ferr := c.detachSharedExperience(ctx, hash, rec, baselineSHA); ferr != nil {
				return res, ferr
			}
			res.Detached = append(res.Detached, rec.ID)
		}
	}
	sort.Slice(res.Kept, func(i, j int) bool { return res.Kept[i] < res.Kept[j] })
	return res, nil
}

// experiencePoolRaw parses one profile.fields.experience array (the raw
// bytes side of experiencePool, so the dedup can read a state the delete
// machine would refuse).
func experiencePoolRaw(raw json.RawMessage) ([]experienceRecord, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("hhapi: experience decode: %w", err)
	}
	records := make([]experienceRecord, 0, len(arr))
	for _, r := range arr {
		var p struct {
			ID      int64 `json:"id"`
			Resumes []struct {
				Hash string `json:"hash"`
			} `json:"resumes"`
		}
		if err := json.Unmarshal(r, &p); err != nil {
			return nil, fmt.Errorf("hhapi: experience record decode: %w", err)
		}
		rec := experienceRecord{Raw: r, ID: p.ID}
		for _, rr := range p.Resumes {
			rec.ResumeHashes = append(rec.ResumeHashes, rr.Hash)
		}
		records = append(records, rec)
	}
	return records, nil
}

// detachSharedExperience detaches one shared record from hash: an edit
// POST with a single trailing-newline tweak makes hh fork the record for
// hash (the fork carries the tweak; the original drops hash). Once the
// fork materializes and the original dropped hash, the fork is deleted
// through the T56 replace route (sole member → not shared).
func (c *Client) detachSharedExperience(ctx context.Context, hash string, rec experienceRecord, baselineSHA map[int64]string) error {
	wantSHA := baselineSHA[rec.ID]
	var base map[string]json.RawMessage
	if err := json.Unmarshal(rec.Raw, &base); err != nil {
		return fmt.Errorf("hhapi: experience dedup decode %d: %w", rec.ID, err)
	}
	delete(base, "resumes") // the save contract never carries usage metadata
	delete(base, "companyLogos")
	var descText string
	if err := json.Unmarshal(base["description"], &descText); err != nil {
		return fmt.Errorf("hhapi: experience dedup decode %d description: %w", rec.ID, err)
	}
	tweaked, merr := json.Marshal(descText + "\n")
	if merr != nil {
		return fmt.Errorf("hhapi: experience dedup encode %d: %w", rec.ID, merr)
	}
	base["description"] = tweaked
	body, merr := json.Marshal([]map[string]json.RawMessage{base})
	if merr != nil {
		return fmt.Errorf("hhapi: experience dedup encode %d: %w", rec.ID, merr)
	}
	if err := c.postResumeEdit(ctx, hash, "experience", body); err != nil {
		return fmt.Errorf("hhapi: experience dedup fork %d: %w", rec.ID, err)
	}

	deadline := time.Now().Add(dedupForkBudget)
	for {
		forkID, originalDropped, originalIntact, err := c.experienceForkState(ctx, hash, rec.ID, descText+"\n", wantSHA)
		if err != nil {
			return err
		}
		if !originalIntact {
			// The shared original's description drifted: hh answered the
			// edit in place instead of forking. Revert and fail loudly.
			return c.revertInPlaceExperience(ctx, hash, rec, wantSHA)
		}
		if forkID != 0 && originalDropped {
			if _, derr := c.DeleteExperienceRecords(ctx, hash, []int64{forkID}); derr != nil {
				return derr
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: record %d after %s", ErrExperienceForkTimeout, rec.ID, dedupForkBudget)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hhapi: experience dedup fork %d: %w", rec.ID, ctx.Err())
		case <-time.After(dedupPollInterval):
		}
	}
}

// experienceForkState polls one round: the fork's id (a record carrying
// the tweaked description that the baseline did not have — it may
// materialize without membership first, the T67 lag window), whether the
// original dropped hash, and whether the original's description still
// matches the baseline sha.
func (c *Client) experienceForkState(ctx context.Context, hash string, originalID int64, tweaked, wantSHA string) (forkID int64, dropped, intact bool, err error) {
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		return 0, false, false, err
	}
	pool, err := experiencePool(st)
	if err != nil {
		return 0, false, false, err
	}
	for _, r := range pool {
		var p struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(r.Raw, &p)
		switch {
		case r.ID == originalID:
			intact = experienceDescSHA(r.Raw) == wantSHA
			dropped = !r.rendersOn(hash)
		case p.Description == tweaked:
			forkID = r.ID
		}
	}
	return forkID, dropped, intact, nil
}

// revertInPlaceExperience restores the shared record's description after
// an in-place edit and verifies the restore; it never leaves the drift
// silent.
func (c *Client) revertInPlaceExperience(ctx context.Context, hash string, rec experienceRecord, wantSHA string) error {
	var base map[string]json.RawMessage
	if err := json.Unmarshal(rec.Raw, &base); err != nil {
		return err
	}
	delete(base, "resumes")
	delete(base, "companyLogos")
	body, merr := json.Marshal([]map[string]json.RawMessage{base})
	if merr != nil {
		return merr
	}
	if err := c.postResumeEdit(ctx, hash, "experience", body); err != nil {
		return fmt.Errorf("%w: record %d revert POST: %v", ErrExperienceInPlace, rec.ID, err)
	}
	rdeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(rdeadline) {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			return err
		}
		pool, err := experiencePool(st)
		if err != nil {
			return err
		}
		for _, r := range pool {
			if r.ID == rec.ID && experienceDescSHA(r.Raw) == wantSHA {
				return fmt.Errorf("%w: record %d reverted", ErrExperienceInPlace, rec.ID)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dedupPollInterval):
		}
	}
	return fmt.Errorf("%w: record %d revert not confirmed", ErrExperienceInPlace, rec.ID)
}

// experienceGroups buckets rendered records by company name in
// deterministic (sorted) key order.
func experienceGroups(records []experienceRecord) [][]experienceRecord {
	keys := make([]string, 0, len(records))
	byKey := map[string][]experienceRecord{}
	for _, rec := range records {
		var p struct {
			Company string `json:"companyName"`
		}
		_ = json.Unmarshal(rec.Raw, &p)
		if _, ok := byKey[p.Company]; !ok {
			keys = append(keys, p.Company)
		}
		byKey[p.Company] = append(byKey[p.Company], rec)
	}
	sort.Strings(keys)
	out := make([][]experienceRecord, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

// experienceIsOrphan reports whether the lone record is a crashed run's
// fork: its description is exactly another rendered record's description
// plus the single tweak newline.
func experienceIsOrphan(rec experienceRecord, group []experienceRecord) bool {
	desc, hasDesc, _ := experienceDescRaw(rec.Raw)
	if !hasDesc {
		return false
	}
	for _, other := range group {
		if other.ID == rec.ID {
			continue
		}
		base, ok, _ := experienceDescRaw(other.Raw)
		if ok && base+"\n" == desc {
			return true
		}
	}
	return false
}

// experienceDescRaw extracts the description with presence (missing field
// is distinct from empty).
func experienceDescRaw(raw json.RawMessage) (desc string, ok bool, sha string) {
	var p struct {
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Description == nil {
		return "", false, ""
	}
	sum := sha256.Sum256([]byte(*p.Description))
	return *p.Description, true, hex.EncodeToString(sum[:])[:12]
}

func experienceDescSHA(raw json.RawMessage) string {
	_, _, sha := experienceDescRaw(raw)
	return sha
}

// rendersOn reports whether the record lists hash in its resumes usage
// list.
func (r experienceRecord) rendersOn(hash string) bool {
	for _, h := range r.ResumeHashes {
		if h == hash {
			return true
		}
	}
	return false
}
