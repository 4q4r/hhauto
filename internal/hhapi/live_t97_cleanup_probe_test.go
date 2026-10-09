//go:build live97

package hhapi

// T97 LIVE CLEANUP PROBE (manual, outside the gate): deletes the orphan
// shell ecb60c97… through the production DeleteResume (BFF host, the
// strict {url} envelope gate) and proves it left the shard. The shell is
// the empty leftover of an interrupted create (owner aware); the probe is
// pinned to THAT hash and refuses anything else — the owner's base
// resumes are untouchable.
//
//	go test -tags live97 -run TestLive97OrphanCleanup -count=1 -v ./internal/hhapi/

import (
	"context"
	"testing"
	"time"
)

// live97OrphanHash is the empty shell left by an interrupted create.
const live97OrphanHash = "ecb60c97ff113b033b0039ed1f38616d4d4e"

func TestLive97OrphanCleanup(t *testing.T) {
	c := live96Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	before := live96DumpShard(t, c, ctx, "t97-shard-before.json")
	orphan, ok := before[live97OrphanHash]
	if !ok {
		t.Fatalf("orphan %s not in the shard — nothing to clean (already deleted?)", live97OrphanHash)
	}
	t.Logf("orphan present: title=%q status=%q searchable=%v", orphan.Title, orphan.Status, orphan.Searchable)

	// Guard: the probe deletes ONLY the pinned empty shell. A titled,
	// modified resume on a tiny shard does not look like an orphan.
	if orphan.Status == "modified" && orphan.Title != "" && len(before) <= 3 {
		t.Fatalf("refusing: %s does not look like an orphan shell", live97OrphanHash)
	}

	if err := c.DeleteResume(ctx, live97OrphanHash); err != nil {
		t.Fatalf("DeleteResume: %v", err)
	}
	t.Log("DeleteResume accepted ({url} envelope gate passed)")

	after := live96DumpShard(t, c, ctx, "t97-shard-after.json")
	if _, still := after[live97OrphanHash]; still {
		t.Fatalf("orphan %s STILL in the shard after delete", live97OrphanHash)
	}
	t.Logf("orphan left the shard; shard now holds %d resumes", len(after))
	for h := range after {
		if _, was := before[h]; !was {
			t.Errorf("unexpected new resume %s appeared during cleanup", h)
		}
	}
	t.Log("live97 orphan cleanup: DONE")
}
