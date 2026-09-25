package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// T26 resume cloning (live-verified 2026-09-25):
//
//   - POST /applicant/resumes/clone, XHR headers + X-Xsrftoken, JSON body
//     {"resumeId":"<id>"} → 200 {"url":"/profile/resume/?resume=<NEW_HASH>"};
//     the new resume hash is parsed from that url.
//   - After a clone the /shards/applicant/resumes entry carries
//     status="not_finished" and NO parentResumeId: hh does not expose the
//     clone parent, so lineage is tracked locally (internal/store).

// ErrResumeLimit is the hh resume-count quota rejection (T26).
// It extends the contract sentinels (same pattern as ErrXSRF in
// errors.go; contracts.go stays untouched). The live rejection shape is
// unknown — the marker scan below is deliberately narrow (validation or
// forbidden class plus an explicit limit word) so ordinary 4xx never
// masquerades as a limit.
var ErrResumeLimit = errors.New("hhapi: resume limit reached")

// resumeLimitMarkers: candidate hh resume-quota rejection markers (RU + EN).
var resumeLimitMarkers = []string{"limit", "лимит", "превышен", "максимум"}

// ResumeCloner is the optional autoclone capability of the site client.
// *Client implements it; feature packages consume it through a type
// assertion so fakes and alternate transports may omit it.
type ResumeCloner interface {
	CloneResume(ctx context.Context, resumeID string) (newHash string, err error)
}

// CloneResume posts the clone request and returns the new resume hash parsed
// from the response url. The request is a mutation (kindMutation): never
// auto-retried, XSRF-refreshed once on a 403 xsrf rejection. A 200 without a
// parsable resume url is an upstream contract violation, never a silent
// success — the caller cannot distinguish "cloned" from "not cloned" without
// the hash.
func (c *Client) CloneResume(ctx context.Context, resumeID string) (string, error) {
	resp, err := c.doMutation(ctx, func() requestSpec {
		body, _ := json.Marshal(struct {
			ResumeID string `json:"resumeId"`
		}{ResumeID: resumeID})
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "clone_resume",
			method:    http.MethodPost,
			url:       c.endpointURL(epResumeClone, c.eps.siteDefault("/applicant/resumes/clone")),
			header:    h,
			body:      body,
		}
	})
	if err != nil {
		if isResumeLimitResponse(err) {
			return "", &StatusError{Kind: ErrResumeLimit, URL: c.endpointURL(epResumeClone, c.eps.siteDefault("/applicant/resumes/clone"))}
		}
		return "", err
	}
	// JSON captcha verdicts ride a 200 (same as TouchResume).
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return "", &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	hash, perr := parseCloneHash(resp.body)
	if perr != nil {
		return "", &StatusError{Kind: ErrUpstream, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	return hash, nil
}

// isResumeLimitResponse re-tags the transport's 400/422/403 classification as
// ErrResumeLimit when the body carries a limit marker. The StatusError body
// snippet is the masked fragment the pipeline kept — enough for the marker
// scan without re-reading the body.
func isResumeLimitResponse(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	if !errors.Is(se.Kind, ErrValidation) && !errors.Is(se.Kind, ErrForbidden) {
		return false
	}
	return bodyHasAny(se.BodySnippet, resumeLimitMarkers)
}

// parseCloneHash extracts the new resume hash from the live envelope:
// {"url":"/profile/resume/?resume=<hash>"} (absolute urls accepted). Any
// deviation is an error — callers must never proceed without the hash.
func parseCloneHash(body []byte) (string, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("hhapi: clone_resume: decode envelope: %w", err)
	}
	raw := strings.TrimSpace(payload.URL)
	if raw == "" {
		return "", errors.New("hhapi: clone_resume: envelope has no url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("hhapi: clone_resume: parse url: %w", err)
	}
	hash := u.Query().Get("resume")
	if hash == "" {
		return "", errors.New("hhapi: clone_resume: url carries no resume hash")
	}
	return hash, nil
}
