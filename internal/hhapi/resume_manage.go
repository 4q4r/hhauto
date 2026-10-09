package hhapi

// T29 resume management API (visibility + delete), reverse-engineered from
// the deployed applicant_profile bundle (2026-09-25, module 91062 and the
// delete-modal thunk in applicant_profile-route):
//
//	hide:   POST /profile/shards/resume/visibility/hide  {"hash":…,"userPrint":{}}
//	show:   POST /profile/shards/resume/visibility/show  {"hash":…,"userPrint":{}}
//	delete: POST /profile/shards/resume/delete           {"hash":…,"from":"resume-delete","hhtmSource":…}
//	        → 200 {"url": …} (the JS navigates there)
//
// T47 LIVE VERDICT (2026-09-28, owner session, tls-client): delete rides
// the resume-profile-front BFF host, NOT the site host — the site-host
// route is edge-faked for off-browser TLS clients (405 Allow:GET, the T29
// observation reproduced), while the BFF answers
//
//	200 {"url":"/applicant/profile/me"}
//
// and the shell verifiably leaves the resumes shard. DeleteResume
// therefore defaults to the BFF host (resolveExternal, never regional
// pinned) and keeps the {url} envelope gate: an accepted-looking answer
// without the envelope is still refused (fail-safe).
//
// The hide/show twins stay on the site host (their live status was
// inconclusive-but-2xx in T29; re-verify before moving them). Both carry
// the ordinary XHR + xsrf discipline; hhtmSource is the analytics context
// of the calling page.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// resumeDeleteFrom is the literal `from` value hh's own delete modal sends.
const resumeDeleteFrom = "resume-delete"

// resumeDeleteURL is the resume-profile-front BFF delete endpoint (T47
// live-verified 2026-09-28: the BFF answers 200 {"url": …} and the shell
// leaves the shard; the site-host twin is a 405 edge fake). Default used
// verbatim (never regional-host pinned), overridable via resume_delete.
const resumeDeleteURL = "https://resume-profile-front.hh.ru/profile/shards/resume/delete"

// resumeManageHhtmSource is the analytics context the JS caller carries on
// the resume-list page (overridable through endpoint config is NOT needed:
// hh treats it as analytics metadata).
const resumeManageHhtmSource = "resume_list"

// HideResume hides the resume from everyone (visibility no_one). This is
// the hh-native "скрыть" — the resume stays listed in the account shard.
func (c *Client) HideResume(ctx context.Context, hash string) error {
	return c.postResumeManage(ctx, epResumeVisibilityHide, c.eps.siteDefault("/profile/shards/resume/visibility/hide"),
		map[string]json.RawMessage{"hash": mustJSONRaw(hash), "userPrint": json.RawMessage(`{}`)}, "resume_visibility_hide")
}

// ShowResume re-publishes a hidden resume (the rollback lever for hide).
func (c *Client) ShowResume(ctx context.Context, hash string) error {
	return c.postResumeManage(ctx, epResumeVisibilityShow, c.eps.siteDefault("/profile/shards/resume/visibility/show"),
		map[string]json.RawMessage{"hash": mustJSONRaw(hash), "userPrint": json.RawMessage(`{}`)}, "resume_visibility_show")
}

// DeleteResume permanently deletes the resume. The response {url} is the
// redirect target hh's UI navigates to; it is parsed to prove the server
// accepted the mutation (an empty body is an upstream anomaly). T47: the
// route lives on the resume-profile-front BFF host and is resolved
// externally (never regional-host pinned — see resumeDeleteURL).
func (c *Client) DeleteResume(ctx context.Context, hash string) error {
	body := map[string]json.RawMessage{
		"hash":       mustJSONRaw(hash),
		"from":       mustJSONRaw(resumeDeleteFrom),
		"hhtmSource": mustJSONRaw(resumeManageHhtmSource),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("hhapi: resume_delete: encode: %w", err)
	}
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_delete",
			method:    http.MethodPost,
			url:       c.eps.resolveExternal(epResumeDelete, resumeDeleteURL),
			header:    h,
			body:      raw,
		}
	})
	if err != nil {
		return err
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil || out.URL == "" {
		return fmt.Errorf("hhapi: resume_delete: envelope has no redirect url: %v (%s)", err, snippet(resp.body))
	}
	return nil
}

// postResumeManage posts one management mutation on the site host.
func (c *Client) postResumeManage(ctx context.Context, epName, defaultURL string, body map[string]json.RawMessage, op string) error {
	_, err := c.postResumeManageBody(ctx, epName, defaultURL, body, op)
	return err
}

func (c *Client) postResumeManageBody(ctx context.Context, epName, defaultURL string, body map[string]json.RawMessage, op string) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", op, err)
	}
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: op,
			method:    http.MethodPost,
			url:       c.endpointURL(epName, defaultURL),
			header:    h,
			body:      raw,
		}
	})
	if err != nil {
		return nil, err
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return nil, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	return resp.body, nil
}

func mustJSONRaw(s string) json.RawMessage {
	out, _ := json.Marshal(s)
	return out
}
