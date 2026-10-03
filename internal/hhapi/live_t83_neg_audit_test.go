//go:build live

package hhapi

// T83 raw negotiations audit: dump the applicantNegotiations state and
// search EVERY topic for vacancy 137339427 (all keys, not just the typed
// Negotiation fields). Read-only GET.
//
//	go test -tags live -run TestLiveT83NegAudit -v ./internal/hhapi/

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveT83NegAudit(t *testing.T) {
	c := t83NewClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	target := c.withPinnedHost(c.endpointURL(epNegotiations, c.eps.siteDefault("/applicant/negotiations")))
	resp, err := c.getDoc(ctx, "negotiations_t83", target, c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		t.Fatalf("GET negotiations: %v", err)
	}
	if err := os.MkdirAll(t83Dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dump := filepath.Join(t83Dir, "negotiations.html")
	if err := os.WriteFile(dump, resp.body, 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	state, ok := initialState(resp.body)
	if !ok {
		t.Fatalf("no HH-Lux-InitialState (bytes=%d)", len(resp.body))
	}
	an := obj(state["applicantNegotiations"])
	if an == nil {
		t.Fatalf("no applicantNegotiations; state keys: %v", func() []string {
			ks := make([]string, 0, len(state))
			for k := range state {
				ks = append(ks, k)
			}
			return ks
		}())
	}
	topics := arr(an["topicList"])
	t.Logf("applicantNegotiations keys=%d topics=%d", len(an), len(topics))
	for i, raw := range topics {
		m := obj(raw)
		vac := obj(m["vacancy"])
		name := str(vac, "name")
		t.Logf("topic[%d]: keys=%v vacancyId=%v vacancy.name=%.60q", i, len(m), m["vacancyId"], name)
	}
	hits := strings.Count(string(resp.body), fmt.Sprintf("%d", t83VacancyID))
	t.Logf("raw mentions of %d in the page: %d", t83VacancyID, hits)
	b, _ := json.Marshal(state)
	t.Logf("state bytes=%d mentions=%d", len(b), strings.Count(string(b), fmt.Sprintf("%d", t83VacancyID)))
}
