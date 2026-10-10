//go:build live123

package hhapi

// T123 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live123 -run TestLive123DiscardProbe -count=1 -v ./internal/hhapi/
//
// Read-only: dumps the SSR discard-filtered negotiations pages and surfaces
// the state shape (state keys, applicantNegotiations topicList size) so a
// zero-item parse can be told apart from a contract drift. Raw pages land
// in /tmp/opencode/t123/. No POSTs here.

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

func live123Client(t *testing.T) *Client {
	t.Helper()
	cfg, err := config.Load("/tmp/opencode/t123/hhauto.toml")
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
	return c
}

func TestLive123DiscardProbe(t *testing.T) {
	c := live123Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, tc := range []struct{ name, suffix string }{
		{"discard-p1", "/applicant/negotiations?filter=discard"},
		{"discard-p2", "/applicant/negotiations?filter=discard&page=2"},
		{"all-p1", "/applicant/negotiations"},
	} {
		resp, err := c.getDoc(ctx, "negotiations", c.withPinnedHost(c.eps.siteDefault(tc.suffix)), c.withPinnedHost(c.eps.site+"/"))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		path := "/tmp/opencode/t123/" + tc.name + ".html"
		if werr := os.WriteFile(path, resp.body, 0o600); werr != nil {
			t.Fatalf("write %s: %v", path, werr)
		}
		state, ok := initialState(resp.body)
		if !ok {
			t.Logf("%s: NO initial state (len=%d)", tc.name, len(resp.body))
			continue
		}
		keys := make([]string, 0, len(state))
		for k := range state {
			keys = append(keys, k)
		}
		t.Logf("%s: state keys = %v", tc.name, keys)
		if an := obj(state["applicantNegotiations"]); an != nil {
			topics := arr(an["topicList"])
			t.Logf("%s: applicantNegotiations.topicList = %d items", tc.name, len(topics))
			for i, raw := range topics {
				if i >= 5 {
					break
				}
				m := obj(raw)
				t.Logf("  item %d: id=%v vacancyId=%v name=%v state=%v updatedAt=%v",
					i, m["id"], m["vacancyId"], m["vacancyName"], m["lastState"], m["updatedAt"])
			}
			// Surface every numeric-ish sibling so a renamed node is visible.
			for k, v := range an {
				if k == "topicList" {
					continue
				}
				if s, serr := json.Marshal(v); serr == nil && len(s) < 300 {
					t.Logf("  applicantNegotiations.%s = %s", k, s)
				}
			}
		} else {
			t.Logf("%s: applicantNegotiations ABSENT", tc.name)
		}
	}
}
