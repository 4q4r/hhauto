//go:build live99

package hhapi

// T99 post-timeout state check (read-only): did the fork from the
// interrupted apply materialize, did the original drift, is the membership
// unchanged?

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLive99StateCheck(t *testing.T) {
	c, cancel := live99Client(t)
	defer cancel()
	ctx := context.Background()
	const hash = "38549492ff113b0d0b0039ed1f636362577a45"
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	pool, err := experiencePool(st)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Logf("pool=%d", len(pool))
	for _, rec := range pool {
		var p struct {
			Company     string `json:"companyName"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(rec.Raw, &p)
		member := rec.rendersOn(hash)
		t.Logf("  %d | %-16s | member=%-5v | desc_sha=%s | desc_tail=%q",
			rec.ID, p.Company, member, experienceDescSHA(rec.Raw), tail(p.Description, 24))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
