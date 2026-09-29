package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// T56 live contract (captured 2026-09-29 from the real editor UI flow
// /profile/edit/experience/{id}?resumeFrom={hash}, cloakbrowser): deleting
// experience records is NOT a dedicated endpoint but a REPLACE update of
// the lineage experience pool:
//
//	POST https://resume-profile-front.hh.ru/profile/shards/profile/update
//	{"profile":{"experience":[<every remaining record>]},
//	 "resumeFromHash":"<hash>"}
//	+ X-hhtmSource: profile_experience, X-hhtmFrom: resume,
//	  X-hhtmSourceLabel: resume, X-hhtmFromLabel: resume
//
// Record ids missing from the array are deleted; ids present ride verbatim
// (raw bytes from the state). The experience list in get_by_hash
// profile.fields.experience is the LINEAGE POOL: every resume of the
// lineage sees all pool records, and each record's resumes list names the
// resumes that render it. Records shared across resumes must never be
// deleted by automation.

// profileExperienceUpdateURL is the resume-profile-front service endpoint
// (never regional-host pinned); overridable via config like every endpoint.
const profileExperienceUpdateURL = "https://resume-profile-front.hh.ru/profile/shards/profile/update"

// ErrExperienceShared reports a delete attempt on a record whose resumes
// list mentions other resumes: removing it would change those resumes too.
var ErrExperienceShared = errors.New("hhapi: experience record shared across resumes")

// ErrExperienceDelete reports a replace POST that hh accepted but whose
// read-back shows not applied (accepted-but-ignored saves are a live T55
// failure mode of the edit contract).
var ErrExperienceDelete = errors.New("hhapi: experience delete not applied")

// experienceRecord is one lineage-pool entry kept as raw bytes plus the
// projection delete verification needs.
type experienceRecord struct {
	Raw          json.RawMessage
	ID           int64
	ResumeHashes []string
}

// experiencePool reads the resume's experience pool from the builder state.
func experiencePool(st *builderState) ([]experienceRecord, error) {
	raw, ok := st.ProfileFields["experience"]
	if !ok {
		return nil, errors.New("hhapi: no profile.fields.experience in builder state")
	}
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

// sharedWithOthers reports whether the record renders on resumes other
// than selfHash.
func (r experienceRecord) sharedWithOthers(selfHash string) bool {
	for _, h := range r.ResumeHashes {
		if h != selfHash {
			return true
		}
	}
	return false
}

// DeleteExperienceRecords removes the given experience record ids from
// hash's lineage pool: one replace POST per id, with a fresh read before
// and after each (silent no-ops are the T55 lesson). Absent ids are
// skipped, so the call is idempotent. A record whose resumes list mentions
// any resume other than hash aborts the whole call with ErrExperienceShared
// before the first POST — shared records belong to other resumes too.
// Returns the number of records left in the pool.
func (c *Client) DeleteExperienceRecords(ctx context.Context, hash string, ids []int64) (int, error) {
	pool := func() ([]experienceRecord, error) {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			return nil, err
		}
		return experiencePool(st)
	}

	// Guard pass: refuse shared targets before any mutation.
	guard, err := pool()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		idx := experienceIndexOf(guard, id)
		if idx < 0 {
			continue
		}
		if guard[idx].sharedWithOthers(hash) {
			return 0, fmt.Errorf("%w: id=%d resumes=%v", ErrExperienceShared, id, guard[idx].ResumeHashes)
		}
	}

	for _, id := range ids {
		before, err := pool()
		if err != nil {
			return 0, err
		}
		idx := experienceIndexOf(before, id)
		if idx < 0 {
			continue
		}
		remaining := make([]json.RawMessage, 0, len(before)-1)
		for j, rec := range before {
			if j != idx {
				remaining = append(remaining, rec.Raw)
			}
		}
		body, merr := json.Marshal(map[string]json.RawMessage{
			"profile":        mustRawJSON(map[string]json.RawMessage{"experience": mustRawJSON(remaining)}),
			"resumeFromHash": mustRawJSON(hash),
		})
		if merr != nil {
			return 0, fmt.Errorf("hhapi: experience replace body: %w", merr)
		}
		resp, err := c.doMutation(ctx, func() requestSpec {
			return c.buildExperienceReplace(body)
		})
		if err != nil {
			return 0, err
		}
		if resp.status != http.StatusOK {
			return 0, &StatusError{Kind: ErrUpstream, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
		}
		after, err := pool()
		if err != nil {
			return 0, err
		}
		if len(after) != len(before)-1 || experienceIndexOf(after, id) >= 0 {
			return 0, fmt.Errorf("%w: id=%d pool %d->%d", ErrExperienceDelete, id, len(before), len(after))
		}
	}
	final, err := pool()
	if err != nil {
		return 0, err
	}
	return len(final), nil
}

// buildExperienceReplace renders one profile/shards/profile/update request
// with the header set captured from the live editor delete flow (XSRF is
// stamped by doMutation, cookies and UA by the transport; the hash rides in
// the body's resumeFromHash).
func (c *Client) buildExperienceReplace(body []byte) requestSpec {
	h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/profile/edit/experience"))
	h.Set("Accept", "application/json")
	h.Set("Origin", c.withPinnedHost(c.eps.site))
	h.Set("Sec-Fetch-Site", "same-site")
	h.Set("Content-Type", "application/json")
	h.Set("X-hhtmSource", "profile_experience")
	h.Set("X-hhtmFrom", "resume")
	h.Set("X-hhtmSourceLabel", "resume")
	h.Set("X-hhtmFromLabel", "resume")
	return requestSpec{
		kind:      kindMutation,
		operation: "profile_experience_update",
		method:    http.MethodPost,
		url:       c.endpointURL(epProfileExperienceUpdate, profileExperienceUpdateURL),
		header:    h,
		body:      body,
	}
}

func experienceIndexOf(records []experienceRecord, id int64) int {
	for i, r := range records {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// mustRawJSON marshals v, panicking on failure: every argument is built
// in-process from records hh itself produced, so a marshal error is a
// programming bug, not a runtime condition.
func mustRawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hhapi: marshal in-process value: %v", err))
	}
	return b
}
