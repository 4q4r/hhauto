package hhapi

// T99 created-resume experience dedup. Live mechanism (dumps and the
// 2026-10-09 live experiment, /tmp/opencode/t99/): the account experience
// pool is ONE lineage pool — every record lists in "resumes" the resumes
// that render it, and publishing a created resume attaches its hash to
// EVERY pool record, so the fresh resume renders all generations per
// employer. The edit route's experience array is a RENDER-SET statement:
// posting {"experience":[…]} makes hh render exactly the listed records
// on this resume (every unlisted record drops the resume's hash) while
// the listed records' content upserts by id — unchanged content is a
// no-op (live proof: one single-record POST collapsed the resume's
// renders to exactly that record, pool ids and descriptions
// byte-unchanged, no fork). The pool itself is never touched: no record
// is created or deleted, other resumes' memberships stay as they were.
//
// The dedup therefore posts the keep-set — the newest generation (highest
// record id; end dates are identical within an employer) per company —
// and verifies the pool's integrity around the POST: same record ids,
// same description shas, and every record's membership changed only by
// losing hash. Idempotent: a resume already rendering one generation per
// employer performs no POST.

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

// dedupVerifyBudget bounds the wait for hh to apply the render-set change;
// dedupPollInterval is the poll step. Package variables so tests can
// shrink them.
var (
	dedupVerifyBudget = 300 * time.Second
	dedupPollInterval = 10 * time.Second
)

// ErrExperienceDedupIntegrity reports a render-set POST whose read-back
// broke a pool invariant (a record disappeared, a description drifted, or
// another resume's membership changed) — the account's shared experience
// must never change beyond hash's own render set.
var ErrExperienceDedupIntegrity = errors.New("hhapi: experience dedup broke a pool invariant")

// ErrExperienceDedupVerify reports a render-set POST hh accepted but did
// not apply within the budget (the T55 silent-no-op class).
var ErrExperienceDedupVerify = errors.New("hhapi: experience dedup render set not applied")

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

// experienceDescRaw extracts the description with presence (missing field
// is distinct from empty) plus its sha prefix.
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

// DetachExperienceResult reports one DetachDuplicateExperience run.
type DetachExperienceResult struct {
	// Kept holds one record id per employer — the newest generation left
	// rendering on the resume.
	Kept []int64
	// Detached holds the record ids that stopped rendering on the resume.
	Detached []int64
}

// DetachDuplicateExperience prunes the duplicate experience generations a
// created resume inherited from the account pool: the resume must render
// exactly the newest generation (highest record id; end dates are
// identical within an employer) per pool employer, so the keep set is
// selected across the whole pool and posted once as the render set.
// Idempotent; a resume rendering nothing performs no mutation (the
// create→improve chain runs this before the resume ever rendered pool
// records).
func (c *Client) DetachDuplicateExperience(ctx context.Context, hash string) (DetachExperienceResult, error) {
	res := DetachExperienceResult{Kept: []int64{}, Detached: []int64{}}
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		return res, err
	}
	raw, ok := st.ProfileFields["experience"]
	if !ok {
		// No experience section in the builder state — nothing to prune
		// (the fresh-shell shape; the create→improve chain calls this
		// before the resume ever rendered a pool record).
		return res, nil
	}
	pool, err := experiencePoolRaw(raw)
	if err != nil {
		return res, err
	}

	before := make([]experienceRecord, 0, len(pool))
	rendered := make([]experienceRecord, 0, len(pool))
	for _, rec := range pool {
		before = append(before, rec)
		if rec.rendersOn(hash) {
			rendered = append(rendered, rec)
		}
	}
	if len(rendered) == 0 {
		return res, nil
	}

	// The keep set is selected across the WHOLE pool, not just the
	// currently rendered records: a created resume's legitimate view is
	// the newest generation of every pool employer, and a render set
	// collapsed by an interrupted operation (live: a partial render-set
	// POST) must be restored, not frozen.
	keep := experienceNewestPerCompany(pool)
	for _, rec := range keep {
		res.Kept = append(res.Kept, rec.ID)
	}
	sort.Slice(res.Kept, func(i, j int) bool { return res.Kept[i] < res.Kept[j] })
	if experienceSameIDs(keep, rendered) {
		return res, nil // already one generation per employer
	}

	keepSet := make(map[int64]bool, len(keep))
	for _, rec := range keep {
		keepSet[rec.ID] = true
	}
	for _, rec := range rendered {
		if !keepSet[rec.ID] {
			res.Detached = append(res.Detached, rec.ID)
		}
	}
	sort.Slice(res.Detached, func(i, j int) bool { return res.Detached[i] < res.Detached[j] })

	// The render-set body: the keep records verbatim (minus the usage
	// metadata the save contract never carries), ascending id.
	sort.Slice(keep, func(i, j int) bool { return keep[i].ID < keep[j].ID })
	items := make([]json.RawMessage, 0, len(keep))
	for _, rec := range keep {
		var base map[string]json.RawMessage
		if uerr := json.Unmarshal(rec.Raw, &base); uerr != nil {
			return res, fmt.Errorf("hhapi: experience dedup decode %d: %w", rec.ID, uerr)
		}
		delete(base, "resumes")
		delete(base, "companyLogos")
		b, merr := json.Marshal(base)
		if merr != nil {
			return res, fmt.Errorf("hhapi: experience dedup encode %d: %w", rec.ID, merr)
		}
		items = append(items, b)
	}
	body, merr := json.Marshal(items)
	if merr != nil {
		return res, fmt.Errorf("hhapi: experience dedup encode: %w", merr)
	}
	if err := c.postResumeEdit(ctx, hash, "experience", body); err != nil {
		return res, fmt.Errorf("hhapi: experience dedup render set: %w", err)
	}

	deadline := time.Now().Add(dedupVerifyBudget)
	for {
		applied, ierr := c.experienceDedupVerify(ctx, hash, before, keepSet)
		if ierr != nil {
			return res, ierr
		}
		if applied {
			return res, nil
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("%w: %s after %s", ErrExperienceDedupVerify, hash, dedupVerifyBudget)
		}
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("hhapi: experience dedup verify: %w", ctx.Err())
		case <-time.After(dedupPollInterval):
		}
	}
}

// experienceDedupVerify re-reads the pool and checks the post-POST
// invariants: every record survived with an unchanged description, hash
// renders exactly the keep set, and no other resume's membership moved.
func (c *Client) experienceDedupVerify(ctx context.Context, hash string, before []experienceRecord, keepSet map[int64]bool) (bool, error) {
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		return false, err
	}
	raw, ok := st.ProfileFields["experience"]
	if !ok {
		return false, fmt.Errorf("%w: experience section vanished", ErrExperienceDedupIntegrity)
	}
	pool, err := experiencePoolRaw(raw)
	if err != nil {
		return false, err
	}
	beforeByID := make(map[int64]experienceRecord, len(before))
	for _, rec := range before {
		beforeByID[rec.ID] = rec
	}
	applied := true
	for _, rec := range pool {
		was, known := beforeByID[rec.ID]
		if !known {
			return false, fmt.Errorf("%w: record %d appeared", ErrExperienceDedupIntegrity, rec.ID)
		}
		if experienceDescSHA(rec.Raw) != experienceDescSHA(was.Raw) {
			return false, fmt.Errorf("%w: record %d description drifted", ErrExperienceDedupIntegrity, rec.ID)
		}
		selfMember := rec.rendersOn(hash)
		if selfMember != keepSet[rec.ID] {
			applied = false // own render set not settled yet — keep polling
		}
		for _, member := range was.ResumeHashes {
			if member == hash {
				continue // our own membership is the change we are making
			}
			if !rec.rendersOn(member) {
				return false, fmt.Errorf("%w: record %d lost member %s", ErrExperienceDedupIntegrity, rec.ID, member)
			}
		}
	}
	if len(pool) != len(before) {
		return false, fmt.Errorf("%w: pool %d → %d records", ErrExperienceDedupIntegrity, len(before), len(pool))
	}
	return applied, nil
}

// experienceNewestPerCompany picks, per company name, the highest record
// id (hh's record ids are a monotonic sequence: the newest generation of
// that employer's improve history).
func experienceNewestPerCompany(records []experienceRecord) []experienceRecord {
	keys := make([]string, 0, len(records))
	byKey := map[string]experienceRecord{}
	for _, rec := range records {
		var p struct {
			Company string `json:"companyName"`
		}
		_ = json.Unmarshal(rec.Raw, &p)
		if _, ok := byKey[p.Company]; !ok {
			keys = append(keys, p.Company)
		}
		if best, ok := byKey[p.Company]; !ok || rec.ID > best.ID {
			byKey[p.Company] = rec
		}
	}
	sort.Strings(keys)
	out := make([]experienceRecord, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

// experienceSameIDs reports whether both lists carry the same record ids.
func experienceSameIDs(a, b []experienceRecord) bool {
	if len(a) != len(b) {
		return false
	}
	ids := make(map[int64]bool, len(a))
	for _, rec := range a {
		ids[rec.ID] = true
	}
	for _, rec := range b {
		if !ids[rec.ID] {
			return false
		}
	}
	return true
}
