//go:build live51

package hhapi

// T51 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live51 -run TestLive51EditEndpointProbe -count=1 -v ./internal/hhapi/
//
// Probes the REAL UI save contract captured live on 2026-09-29 from the
// hh.ru resume editor (cloakbrowser session, resume_partial_edit flow):
//
//	POST {site}/applicant/resume/edit?resume={hash}&hhtmSource=resume_partial_edit
//	{"title":["…"]}                      (title-only subset variant)
//	+ X-hhtmSource: resume_partial_edit, X-hhtmFrom: ""
//
// against the LIVE clone HHAUTO_T51_HASH with its CURRENT title (no-op
// content-wise). Success = HTTP 200 builder-state envelope that parses and
// reads back with the same title. Base resumes are never touched.
//
// No secrets are printed; env vars are not required (defaults below).

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const live51DefaultHash = "4f48f357ff112c2fcc0039ed1f6c7555414349"

func TestLive51EditEndpointProbe(t *testing.T) {
	hash := os.Getenv("HHAUTO_T51_HASH")
	if hash == "" {
		hash = live51DefaultHash
	}
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		t.Fatalf("builder state: %v", err)
	}
	title := st.Title()
	if title == "" {
		t.Fatalf("clone %s has no title; refusing to probe", hash)
	}
	t.Logf("live state: title=%q screens=%v status=%q", title, st.Screens, st.Status())

	// Variant A: title-only subset (what the endpoint must accept for a
	// single-field improve save to be possible).
	body, _ := json.Marshal(map[string]json.RawMessage{
		"title": json.RawMessage(`["` + title + `"]`),
	})
	url := c.eps.siteDefault("/applicant/resume/edit") + "?resume=" + hash + "&hhtmSource=resume_partial_edit"
	resp, perr := c.postLive51(ctx, url, body)
	if perr != nil {
		t.Fatalf("variant A (title-only): %v", perr)
	}
	t.Logf("variant A status=%d body[:300]=%s", resp.status, snippet(resp.body))
	if st2, perr := parseBuilderState(resp.body); perr != nil {
		t.Logf("variant A body does not parse as builder state: %v", perr)
	} else {
		t.Logf("variant A read-back title=%q screens=%v", st2.Title(), st2.Screens)
		if st2.Title() != title {
			t.Errorf("variant A: title changed: %q -> %q", title, st2.Title())
		}
	}

	// Field-shape dump for the serializer design (values truncated).
	for _, key := range []string{"title", "skills", "keySkills", "salary", "professionalRole", "travelTime", "workFormats", "employmentForms", "businessTripReadiness"} {
		raw, ok := st.Resume[key]
		if !ok {
			t.Logf("shape %q: <absent>", key)
			continue
		}
		s := string(raw)
		if len(s) > 400 {
			s = s[:400] + "…"
		}
		t.Logf("shape %q: %s", key, s)
	}
	if raw, ok := st.ProfileFields["experience"]; ok {
		s := string(raw)
		if len(s) > 700 {
			s = s[:700] + "…"
		}
		t.Logf("shape profile.experience: %s", s)
	} else {
		t.Logf("shape profile.experience: <absent>")
	}

	// Variant B: validation-error shape probe (empty title — below minlength;
	// hh must reject and change nothing).
	body, _ = json.Marshal(map[string]json.RawMessage{"title": json.RawMessage(`[""]`)})
	resp, perr = c.postLive51(ctx, url, body)
	if perr != nil {
		t.Logf("variant B (empty title): transport verdict %v", perr)
	} else {
		t.Logf("variant B status=%d body[:400]=%s", resp.status, snippet(resp.body))
	}
}

// postLive51 fires one edit POST with the site-host XHR discipline plus the
// partial-edit hhtm headers (the captured UI set minus browser-only values).
func (c *Client) postLive51(ctx context.Context, url string, body []byte) (*response, error) {
	return c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		h.Set("X-hhtmSource", "resume_partial_edit")
		h.Set("X-hhtmFrom", "")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_edit_probe",
			method:    http.MethodPost,
			url:       url,
			header:    h,
			body:      body,
		}
	})
}
