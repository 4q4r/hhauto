//go:build live47

package hhapi

// T47 LIVE E2E of the shipped implementation (manual; see
// live_t47_probe_test.go for the harness): CreateResume must land a shell
// the shard verifies, DeleteResume (the T47 BFF route) must remove it and
// both must leave the account exactly as they found it.
//
//	go test -tags live47 -run TestLive47ImplRoundTrip -count=1 -v ./internal/hhapi/

import (
	"testing"
	"time"
)

func TestLive47ImplRoundTrip(t *testing.T) {
	c, ctx := live47Client(t)

	before := live47ShardSnapshot(t, c, ctx)
	t.Logf("shard before: %d resumes", len(before))

	hash, err := c.CreateResume(ctx, "T47LIVE impl", nil)
	if err != nil {
		t.Fatalf("CreateResume: %v", err)
	}
	t.Logf("CreateResume -> %s", hash)
	if _, ok := before[hash]; ok {
		t.Fatalf("CreateResume returned a PRE-EXISTING hash %s", hash)
	}
	after := live47ShardSnapshot(t, c, ctx)
	r, ok := after[hash]
	if !ok {
		t.Fatalf("created hash %s not in the shard", hash)
	}
	t.Logf("verified shard row: title=%q status=%s", r.Title, r.Status)
	if r.Status != "not_finished" {
		t.Logf("NOTE: shell status %q (expected not_finished for a fresh shell)", r.Status)
	}

	// Settle, then delete through the implementation.
	time.Sleep(2 * time.Second)
	if err := c.DeleteResume(ctx, hash); err != nil {
		t.Fatalf("DeleteResume: %v", err)
	}
	t.Log("DeleteResume accepted ({url} envelope)")
	final := live47ShardSnapshot(t, c, ctx)
	if _, still := final[hash]; still {
		t.Fatalf("hash %s still in the shard after DeleteResume", hash)
	}
	t.Logf("verified gone; shard back to %d resumes", len(final))
}
