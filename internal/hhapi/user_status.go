package hhapi

import (
	"context"
	"net/http"
)

// T118 job_search_status — DONOR-DERIVED contract (hh-auto-responses
// main.go:2092–2129), never captured live. Wire shape:
//
//	POST {host}/profile/shards/user_statuses/job_search_status?status=looking_for_offers
//	empty body; Accept: application/json; X-Requested-With; XSRF token;
//	X-hhtmSource: resume_list
//
// The donor resolves the host from ExternalMicroFrontendHosts.ResumeProfileFront
// (the resume-profile-front BFF), which is also where every other
// /profile/shards/** mutation of this client defaults (T47/T56) — so the
// default here is the BFF host. An endpoint override ("job_search_status")
// repoints it at the site host (or anywhere) without a code change. The
// spec text said "{site}"; the donor wire and the T108 memory note (BFF)
// disagree — the live probe the feature flag requires settles it.
//
// Unproven claims that must NOT be trusted here: server-side idempotence
// and the "~2 weeks" status lifetime are donor folklore (D6). The captcha
// markers mirror TouchResume: hh answers captcha verdicts on a 200 JSON
// body and via the 302 wall.
const jobSearchStatusURL = "https://resume-profile-front.hh.ru/profile/shards/user_statuses/job_search_status"

// jobSearchStatusLooking is the only status value the donor wire shows.
const jobSearchStatusLooking = "looking_for_offers"

// SetJobSearchStatus implements hhapi.HHAPI: flips the account's
// «Ищу работу» status to looking_for_offers (account-wide — the wire has
// no resume parameter). Callers gate on [features.jobsearch]; the flag
// ships disabled until the owner's live probe succeeds.
func (c *Client) SetJobSearchStatus(ctx context.Context) error {
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Accept", "application/json")
		h.Set("X-hhtmSource", "resume_list")
		return requestSpec{
			kind: kindMutation, operation: "job_search_status",
			method: http.MethodPost,
			// Query rides the resolved URL (Apply's concat shape); an
			// override URL must carry or receive it as written.
			url:    c.endpointURL(epJobSearchStatus, jobSearchStatusURL) + "?status=" + jobSearchStatusLooking,
			header: h,
		}
	})
	if err != nil {
		return err
	}
	// JSON captcha verdicts ride a 200 (donor-verified hhcaptcha.isBot,
	// same TouchResume discipline).
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	return nil
}
