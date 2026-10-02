package hhapi

// T47 resume creation, live-verified 2026-09-28 (owner session, tls-client):
//
//	POST https://resume-profile-front.hh.ru/profile/shards/resume/create
//	  {"resumesCreationProperties":[{"title":"…","roleIds":[…]}],"userPrint":{}}
//	  or {"userPrint":{}} for the empty shell
//	  → 200 {"nextIncompleteScreenId":"professional_role","screens":[…]}
//
// Live facts fixed by the probe (live_t47_probe_test.go, tag live47):
//   - the route exists ONLY on the resume-profile-front BFF host; the
//     site-host twins (/profile/shards/resume/create, /shards/resume/create,
//     /applicant/resumes/create) answer 405/404;
//   - the 200 envelope carries NO resume hash — the new shell is found by
//     diffing the resumes shard (every accepted create added exactly one
//     not_finished entry);
//   - hh IGNORES the content fields at create time (three body shapes
//     probed — creationProperties, builder fields, condition-format title;
//     the shell always stayed untitled with the wizard parked at the
//     professional_role screen). Title/roles therefore ride the wizard
//     (browser editor, T30) or `resume improve` — CreateResume sends them
//     verbatim for forward compatibility but the caller must verify and
//     fill content separately.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// resumeCreateURL is the resume-profile-front BFF create endpoint. Same
// service host and header discipline as clone_resumes/update/delete; the
// default is used verbatim (never regional-host pinned) and stays
// overridable through the resume_create endpoint config key.
const resumeCreateURL = "https://resume-profile-front.hh.ru/profile/shards/resume/create"

// CreateResume creates a fresh resume and returns its hash after the shard
// verifies the new entry. An empty title creates the bare not_finished
// shell (hh's own "Создать резюме" first step); title/roleIDs ride the
// creationProperties shape but are IGNORED by hh at create time (see the
// file doc) — content belongs to the wizard or the improve pipeline.
//
// Errors: typed transport/mutation errors from the pipeline; a 2xx create
// that never appears in the shard is a silent drop and returns a
// StatusError (ErrUpstream) — the caller never gets an invented hash.
func (c *Client) CreateResume(ctx context.Context, title string, roleIDs []string) (string, error) {
	// Pre-read: the verification baseline. Without it a 2xx create cannot
	// be told apart from a silent drop.
	before, err := c.ListResumes(ctx)
	if err != nil {
		return "", fmt.Errorf("hhapi: resume_create: pre-read: %w", err)
	}
	seen := make(map[string]bool, len(before))
	for _, r := range before {
		seen[r.HHID] = true
	}

	body, err := json.Marshal(resumeCreateRequest{
		CreationProps: creationPropsFor(title, roleIDs),
		UserPrint:     json.RawMessage(`{}`),
	})
	if err != nil {
		return "", fmt.Errorf("hhapi: resume_create: encode: %w", err)
	}
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.cloneResumesHeaders()
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_create",
			method:    http.MethodPost,
			url:       c.eps.resolveExternal(epResumeCreate, resumeCreateURL),
			header:    h,
			body:      body,
		}
	})
	if err != nil {
		return "", err
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return "", &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}

	// Verify through the shard: pick the newest entry the account did not
	// have before the create (the envelope carries no hash).
	after, err := c.ListResumes(ctx)
	if err != nil {
		return "", fmt.Errorf("hhapi: resume_create: verify read: %w", err)
	}
	newest := ""
	var newestTS int64
	for _, r := range after {
		if seen[r.HHID] {
			continue
		}
		ts := r.CreatedAt.UnixMilli()
		if ts == 0 {
			ts = r.UpdatedAt.UnixMilli()
		}
		if newest == "" || ts >= newestTS {
			newest, newestTS = r.HHID, ts
		}
	}
	if newest == "" {
		return "", &StatusError{
			Kind:        ErrUpstream,
			StatusCode:  resp.status,
			URL:         resp.finalURL,
			BodySnippet: snippet(resp.body),
			Chain:       resp.chain,
		}
	}
	return newest, nil
}

// resumeCreateRequest is the create body. CreationProps stays absent for
// the empty-shell create (live-verified shape {"userPrint":{}}).
type resumeCreateRequest struct {
	CreationProps []cloneResumePropertySpec `json:"resumesCreationProperties,omitempty"`
	UserPrint     json.RawMessage           `json:"userPrint"`
}

// creationPropsFor builds the creation-properties entry only when there is
// content to forward (title or role ids).
func creationPropsFor(title string, roleIDs []string) []cloneResumePropertySpec {
	if title == "" && len(roleIDs) == 0 {
		return nil
	}
	return []cloneResumePropertySpec{{Title: title, RoleIDs: roleIDs}}
}
