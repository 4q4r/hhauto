//go:build live47

package hhapi

// T47 DELETE-CONTRACT PROBE (manual; see live_t47_probe_test.go for the
// harness): the create ladder left three shells and the delete leg ended
// inconclusive — the T29 endpoint answered 2xx with an EMPTY body (the
// strict {url} envelope gate refused it) and the shell re-check was
// skipped on that path. This run:
//
//  1. inventories the shard — are the ladder shells still there?
//  2. walks delete variants against ONLY those shells (never a base
//     resume): retry site-host T29 delete, BFF-host delete, and logs every
//     envelope verbatim; after each accepted-looking answer the shard is
//     re-read and the presence verdict logged;
//  3. only when a delete path is PROVEN (shell disappears) fires one
//     titled-create variant (condition-format title, the builder's own
//     value shape) and deletes the result — so the account is never left
//     with litter that cannot be removed.
//
//	go test -tags live47 -run TestLive47DeleteContract -count=1 -v ./internal/hhapi/

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Ladder shells from the earlier run (TestLive47CreateAndDelete, this
// branch) — all created BY the probe, all read back status=not_finished.
var live47LadderShells = []string{
	"8ca53abcff112c34820039ed1f67766164366c",
	"d52123baff112c34880039ed1f566246504f42",
	"9652b32eff112c348e0039ed1f465677435073",
}

func TestLive47DeleteContract(t *testing.T) {
	c, ctx := live47Client(t)

	shard := live47ShardSnapshot(t, c, ctx)
	t.Logf("shard now holds %d resumes", len(shard))
	var targets []string
	for _, h := range live47LadderShells {
		r, ok := shard[h]
		if !ok {
			t.Logf("shell %s: already gone", h)
			continue
		}
		if r.Status != "not_finished" {
			t.Fatalf("refusing to probe delete on %s: status %q", h, r.Status)
		}
		t.Logf("shell %s present (title %q)", h, r.Title)
		targets = append(targets, h)
	}
	if len(targets) == 0 {
		t.Log("nothing to probe: the earlier empty-2xx delete actually removed the shells")
		return
	}

	// Delete-variant ladder. Every candidate is fired against the FIRST
	// target only; the winner is then reused for the rest.
	bff := "https://resume-profile-front.hh.ru"
	type dvar struct {
		name string
		spec requestSpec
	}
	mkJSON := func(method, urlStr, body string) requestSpec {
		spec := requestSpec{
			kind:      kindMutation,
			operation: "live47_delete",
			method:    method,
			url:       urlStr,
			header:    c.cloneResumesHeaders(),
			body:      []byte(body),
		}
		spec.header.Set("Content-Type", "application/json")
		if tok := c.xsrfToken(); tok != "" {
			spec.header.Set("X-XSRFToken", tok)
		}
		return spec
	}
	form := func(urlStr string, vals string) requestSpec {
		spec := requestSpec{
			kind:      kindMutation,
			operation: "live47_delete",
			method:    http.MethodPost,
			url:       urlStr,
			header:    c.xhrHeaders(c.eps.site + "/applicant/resumes"),
			body:      []byte(vals),
		}
		spec.header.Set("Content-Type", "application/x-www-form-urlencoded")
		if tok := c.xsrfToken(); tok != "" {
			spec.header.Set("X-XSRFToken", tok)
		}
		return spec
	}
	h0 := targets[0]
	variants := []dvar{
		{"retry site {hash,from,hhtmSource}", mkJSON(http.MethodPost, c.eps.site+"/profile/shards/resume/delete", fmt.Sprintf(`{"hash":%q,"from":"resume-delete","hhtmSource":"resume_list"}`, h0))},
		{"bff {hash,from,hhtmSource}", mkJSON(http.MethodPost, bff+"/profile/shards/resume/delete", fmt.Sprintf(`{"hash":%q,"from":"resume-delete","hhtmSource":"resume_list"}`, h0))},
		{"bff {resumeId}", mkJSON(http.MethodPost, bff+"/profile/shards/resume/delete", fmt.Sprintf(`{"resumeId":%q}`, h0))},
		{"site form resume=hash", form(c.eps.site+"/applicant/resumes/delete", "resume="+h0+"&from=resume-delete")},
	}

	winner := -1
	for i, v := range variants {
		resp, err := c.do(ctx, v.spec)
		if err != nil {
			t.Logf("%s: transport/typed error: %v", v.name, err)
			continue
		}
		body := string(resp.body)
		if len(body) > 200 {
			body = body[:200]
		}
		t.Logf("%s -> %d %q", v.name, resp.status, body)
		if resp.status >= 300 {
			continue
		}
		// Accepted-looking: re-read the shard for the verdict.
		time.Sleep(2 * time.Second) // hh's shard view settles briefly after a delete
		after := live47ShardSnapshot(t, c, ctx)
		if _, still := after[h0]; still {
			t.Logf("  %s: 2xx but the shell is STILL present (silent drop)", v.name)
			continue
		}
		t.Logf("  %s: VERIFIED — shell gone from the shard", v.name)
		winner = i
		break
	}
	if winner < 0 {
		t.Log("NO delete variant verified; leaving remaining shells untouched (report as litter)")
		return
	}

	// Reap the rest with the proven variant, retargeting the payload per
	// shell (JSON shapes interpolate the hash; the form shape rebuilds the
	// query string).
	reap := func(hash string) error {
		var spec requestSpec
		switch {
		case strings.Contains(variants[winner].name, "bff {resumeId}"):
			spec = mkJSON(http.MethodPost, bff+"/profile/shards/resume/delete", fmt.Sprintf(`{"resumeId":%q}`, hash))
		case strings.Contains(variants[winner].name, "bff"):
			spec = mkJSON(http.MethodPost, bff+"/profile/shards/resume/delete", fmt.Sprintf(`{"hash":%q,"from":"resume-delete","hhtmSource":"resume_list"}`, hash))
		case strings.Contains(variants[winner].name, "form"):
			spec = form(c.eps.site+"/applicant/resumes/delete", "resume="+hash+"&from=resume-delete")
		default: // site JSON
			spec = mkJSON(http.MethodPost, c.eps.site+"/profile/shards/resume/delete", fmt.Sprintf(`{"hash":%q,"from":"resume-delete","hhtmSource":"resume_list"}`, hash))
		}
		resp, err := c.do(ctx, spec)
		if err != nil {
			return err
		}
		b := string(resp.body)
		if len(b) > 120 {
			b = b[:120]
		}
		t.Logf("  reap %s -> %d %q", hash, resp.status, b)
		return nil
	}
	for _, h := range targets[1:] {
		if err := reap(h); err != nil {
			t.Logf("  reap %s failed: %v", h, err)
			continue
		}
	}
	for _, h := range targets[1:] {
		after := live47ShardSnapshot(t, c, ctx)
		if _, still := after[h]; still {
			t.Errorf("shell %s survived the proven variant", h)
		} else {
			t.Logf("shell %s gone", h)
		}
	}

	// A proven delete unlocks the titled-create question: fire ONE
	// condition-format create and remove the result.
	title := fmt.Sprintf("T47LIVE titled %d", time.Now().Unix())
	body, _ := json.Marshal(map[string]any{
		"title":     []map[string]string{{"string": title}},
		"userPrint": json.RawMessage(`{}`),
	})
	spec := mkJSON(http.MethodPost, bff+"/profile/shards/resume/create", string(body))
	resp, err := c.do(ctx, spec)
	if err != nil {
		t.Logf("titled create (condition format): %v", err)
		return
	}
	b := string(resp.body)
	if len(b) > 160 {
		b = b[:160]
	}
	t.Logf("titled create -> %d %s", resp.status, b)
	after := live47ShardSnapshot(t, c, ctx)
	var newHash string
	for h, r := range after {
		if strings.Contains(r.Title, "T47LIVE") {
			newHash = h
			t.Logf("titled shell landed: hash=%s title=%q status=%s", h, r.Title, r.Status)
		}
	}
	if newHash == "" {
		t.Log("titled create did NOT apply the title (shell empty again) — create ignores content, the wizard owns it")
		return
	}
	// Clean it up with the proven variant (retarget the payload).
	cleanup := reap(newHash)
	if cleanup != nil {
		t.Errorf("cleanup of the titled shell failed: %v", cleanup)
		return
	}
	final := live47ShardSnapshot(t, c, ctx)
	if _, still := final[newHash]; still {
		t.Errorf("titled shell %s survived cleanup", newHash)
	} else {
		t.Log("titled shell cleaned up — account back to its pre-probe state")
	}
}
