package hhapi

// T29 resume content mutation (the cracked builder update path).
//
// The payload is the SPA-exact serialization of the editor's save thunk,
// reverse-engineered from the deployed resume_profile_front bundle
// (2026-09-25, chunk 6069 module 11154 thunk P, cleanForm module 867, hM
// split module 64673, api layer module 88874):
//
//	POST https://resume-profile-front.hh.ru/profile/shards/resume/update
//	{"resumeHash":"…","currentScreenId":"…",
//	 "resume":{…content fields, condition format…},
//	 "profile":{…personal fields + experience…},
//	 "questionToAnswerMap":{},"additionalProperties":{"anyJob":false},
//	 "userSkillLevels":{},"userPrint":{}}
//
// with the header set T28 proved mandatory (Accept/Content-Type JSON,
// X-Requested-With, X-Xsrftoken, X-hhtm* — see cloneResumesHeaders).
// The split follows hM (module 64673): the personal fields plus experience
// ride the profile side, the content fields stay on resume; identity
// metadata (_attributes/_conditions) never travels.
//
// LIVE STATUS (2026-09-25 probe, -tags live29): hh still rejects the
// update POST off-browser with a bare pre-app 400 for every body variant
// (exact SPA reconstruction, wizard-correct screen ids from the live
// state, live additionalProperties shape, minimal diffs, primed ddg
// cookies, Chrome UA) — and the site-host route answers the 405 edge fake.
// The same wall covers /profile/shards/** POSTs on hh.ru (delete probe:
// 405 Allow:GET). Only the BFF clone_resumes POST passes (T28). The
// contract here stays the decoded truth: when the route opens (or a
// real-browser runner executes it), the code works unchanged; until then
// callers see the typed errors and the feature degrades per field.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// additionalProps is the SPA's additionalProperties body for a plain
// editor save: anyJob rides false unless the any-job toggle is on
// (module 64673 jO: "boolean"==typeof e?e:r||!1).
const additionalProps = `{"anyJob":false}`

// ResumeContent is the mutable content split of one resume: Resume holds
// the condition-format content fields (title, professionalRole, keySkills,
// skills/О себе, salary, workSchedule, …), Profile the personal fields and
// experience. Screens carries the editor screen ids of the resume;
// Conditions the raw hh validation conditions (_conditions) the feature
// validators read live limits from. The maps are plain JSON values ready
// to be mutated field by field and sent back through UpdateResumeContent.
type ResumeContent struct {
	Resume     map[string]json.RawMessage
	Profile    map[string]json.RawMessage
	Screens    []string
	Conditions map[string]json.RawMessage
}

// clone returns a deep-enough copy (top-level keys copied, raw values are
// immutable JSON) so callers can mutate one field without aliasing.
func (rc *ResumeContent) clone() *ResumeContent {
	out := &ResumeContent{
		Resume:     make(map[string]json.RawMessage, len(rc.Resume)),
		Profile:    make(map[string]json.RawMessage, len(rc.Profile)),
		Screens:    append([]string(nil), rc.Screens...),
		Conditions: map[string]json.RawMessage{},
	}
	for k, v := range rc.Conditions {
		out.Conditions[k] = v
	}
	for k, v := range rc.Resume {
		out.Resume[k] = v
	}
	for k, v := range rc.Profile {
		out.Profile[k] = v
	}
	return out
}

// ResumeUpdateValidationError is a hh validation/business rejection of one
// update POST: Codes carries field:code pairs ("title:DUPLICATE").
type ResumeUpdateValidationError struct {
	Codes []string
	Err   error
}

func (e *ResumeUpdateValidationError) Error() string {
	return fmt.Sprintf("hhapi: resume update validation: %v", e.Codes)
}

func (e *ResumeUpdateValidationError) Unwrap() error { return e.Err }

// ResumeContentVerifyError is the verify-gate failure: hh accepted the
// update POSTs but the read-back does not carry the sent values, so
// nothing may claim the change landed.
type ResumeContentVerifyError struct {
	Keys []string
	Err  error
}

func (e *ResumeContentVerifyError) Error() string {
	return fmt.Sprintf("hhapi: resume update verify failed for %v", e.Keys)
}

func (e *ResumeContentVerifyError) Unwrap() error { return e.Err }

// ResumeContentEditor is the T29 content-mutation seam: read the mutable
// split of one resume, post a modified split through the builder update
// walk with the verify gate. *Client implements it; feature fakes may
// substitute their own.
type ResumeContentEditor interface {
	ResumeContent(ctx context.Context, hash string) (*ResumeContent, error)
	UpdateResumeContent(ctx context.Context, hash string, content *ResumeContent, screens []string) error
}

var _ ResumeContentEditor = (*Client)(nil)

// Clone returns an independent copy of the split (mutation-safe).
func (rc *ResumeContent) Clone() *ResumeContent { return rc.clone() }

// ResumeContent reads the mutable content split of one resume (live
// get_by_hash shard, retryable like every read).
func (c *Client) ResumeContent(ctx context.Context, hash string) (*ResumeContent, error) {
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		return nil, err
	}
	resume, profile := splitBuilderResume(st)
	conds := map[string]json.RawMessage{}
	if raw, ok := st.Resume["_conditions"]; ok {
		_ = json.Unmarshal(raw, &conds)
	}
	return &ResumeContent{Resume: resume, Profile: profile, Screens: st.Screens, Conditions: conds}, nil
}

// UpdateResumeContent posts a modified content split through the builder
// update endpoint (per screen in screens, one POST each) and verifies by
// read-back that every changed key landed. Changed keys are computed
// against the LIVE state first: an unchanged content posts nothing.
// Validation rejections surface as *ResumeUpdateValidationError (the
// feature retries with a fixed value or skips the field); a verify
// mismatch is *ResumeContentVerifyError.
func (c *Client) UpdateResumeContent(ctx context.Context, hash string, content *ResumeContent, screens []string) error {
	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		return fmt.Errorf("live content: %w", err)
	}
	changed := diffContentKeys(live, content)
	if len(changed) == 0 {
		return nil
	}
	if len(screens) == 0 {
		if len(content.Screens) > 0 {
			screens = content.Screens
		} else {
			screens = []string{"professional_role"}
		}
	}
	payload := resumeScreenUpdate{
		ResumeHash:      hash,
		Resume:          content.Resume,
		Profile:         content.Profile,
		QuestionMap:     map[string]string{},
		Additional:      json.RawMessage(additionalProps),
		UserSkillLevels: json.RawMessage(`{}`),
		UserPrint:       json.RawMessage(`{}`),
	}
	for _, screen := range screens {
		payload.CurrentScreenID = screen
		if _, verr := c.postResumeScreen(ctx, &payload); verr != nil {
			return verr
		}
	}
	// Verify read-back: every changed key must now equal the sent value.
	verify, err := c.ResumeContent(ctx, hash)
	if err != nil {
		return fmt.Errorf("verify content: %w", err)
	}
	var missing []string
	for _, key := range changed {
		side, name := splitContentKey(key)
		if !rawEqual(sideOf(verify, side)[name], sideOf(content, side)[name]) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return &ResumeContentVerifyError{Keys: missing, Err: ErrCloneMaterialize}
	}
	return nil
}

// postResumeScreen issues one builder update with the SPA-exact header set
// and payload (see resume_content.go for the decoded contract). done
// reports hh's nextIncompleteScreenId == null (complete). Validation
// rejections surface as *ResumeUpdateValidationError; transport losses keep
// the mutation-path semantics (never auto-retried).
func (c *Client) postResumeScreen(ctx context.Context, payload *resumeScreenUpdate) (done bool, err error) {
	body, merr := json.Marshal(payload)
	if merr != nil {
		return false, fmt.Errorf("encode update: %w", merr)
	}
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.cloneResumesHeaders()
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_builder_update",
			method:    http.MethodPost,
			url:       c.resolveUpdateURL(),
			header:    h,
			body:      body,
		}
	})
	if err != nil {
		// Transport-level failure (non-2xx classification, session loss):
		// keep the T27 semantics — the caller sees a materialize failure
		// with the original StatusError in the chain.
		return false, fmt.Errorf("%w: %v", ErrCloneMaterialize, err)
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return false, fmt.Errorf("%w: %v", ErrCloneMaterialize, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain})
	}
	var out resumeUpdateResp
	if uerr := json.Unmarshal(resp.body, &out); uerr != nil {
		return false, fmt.Errorf("%w: decode response: %v", ErrCloneMaterialize, uerr)
	}
	if len(out.Error) > 0 {
		keys := make([]string, 0, len(out.Error))
		for _, e := range out.Error {
			keys = append(keys, e.Key)
		}
		return false, fmt.Errorf("%w: builder error: %s", ErrCloneMaterialize, strings.Join(keys, ","))
	}
	if len(out.Errors) > 0 {
		codes := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			codes = append(codes, e.Field+":"+e.Code)
		}
		return false, &ResumeUpdateValidationError{Codes: codes, Err: ErrCloneMaterialize}
	}
	return out.NextIncomplete == nil, nil
}

// resolveUpdateURL: the update POST belongs to the resume-profile-front
// service (T28 axios wiring — every editor POST rides the BFF host); the
// endpoint stays config-overridable.
func (c *Client) resolveUpdateURL() string {
	return c.eps.resolveExternal(epResumeUpdate, resumeUpdateURL)
}

// diffContentKeys returns "side.name" keys whose values differ between the
// live and the new content.
func diffContentKeys(live, next *ResumeContent) []string {
	var keys []string
	for side, lm := range map[string]map[string]json.RawMessage{"resume": live.Resume, "profile": live.Profile} {
		nm := sideOf(next, side)
		for k, lv := range lm {
			if nv, ok := nm[k]; !ok || !rawEqual(lv, nv) {
				keys = append(keys, side+"."+k)
			}
		}
		for k := range nm {
			if _, ok := lm[k]; !ok {
				keys = append(keys, side+"."+k)
			}
		}
	}
	return keys
}

func sideOf(rc *ResumeContent, side string) map[string]json.RawMessage {
	if side == "profile" {
		return rc.Profile
	}
	return rc.Resume
}

func splitContentKey(key string) (side, name string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:]
		}
	}
	return "resume", key
}

func rawEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return jsonEqual(av, bv)
}

// jsonEqual compares decoded JSON values (maps ignore key order).
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvi, ok := bv[k]
			if !ok || !jsonEqual(v, bvi) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
