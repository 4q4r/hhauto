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
	"encoding/json"
	"testing"
	"time"
)

// live97OrphanHash is the empty shell left by an interrupted create
// (mandate cited the 36-char prefix; this is the full shard hash).
const live97OrphanHash = "ecb60c97ff113b033b0039ed1f38616d4d4e34"

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

// live97RequestedTitles are the owner-authored positionings for the three
// resumes the buggy chain produced (T97 mandate, item 2). The improve fact
// alone cannot retake a DRIFTED title — the model anchors on CurrentValue
// (T92 §7 predicted; live proved 2026-10-09 on 38549492) — so the requested
// titles apply deterministically through the T51 edit contract (the same
// apply+verify the improve pipeline uses), and the improve passes then
// re-ground about/key_skills on the corrected title.
var live97RequestedTitles = map[string]string{
	"38549492ff113b0d0b0039ed1f636362577a45": "Специалист по информационной безопасности",
	"068e9047ff113b0ec00039ed1f357a626b314e": "QA Automation Engineer (Python)",
	"a88bd526ff113b10be0039ed1f776342455439": "Преподаватель Python и информационной безопасности",
}

// titleTextOfBuilder unwraps the builder title condition to the plain
// string (probe logging/compare only).
func titleTextOfBuilder(raw json.RawMessage) string {
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return ""
	}
	s, _ := parts[0]["string"].(string)
	return s
}

// TestLive97ApplyRequestedTitles applies the requested titles verbatim
// (T51 upsert-by-id edit + built-in read-back verify). Idempotent: an
// already-correct title is skipped. Pinned hashes only.
func TestLive97ApplyRequestedTitles(t *testing.T) {
	c := live96Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	for hash, want := range live97RequestedTitles {
		live, err := c.ResumeContent(ctx, hash)
		if err != nil {
			t.Fatalf("%s: live content: %v", hash, err)
		}
		if got := titleTextOfBuilder(live.Resume["title"]); got == want {
			t.Logf("%s: title already %q — skip", hash, want)
			continue
		} else {
			t.Logf("%s: title %q -> %q", hash, got, want)
		}
		cond, _ := json.Marshal([]map[string]string{{"string": want}})
		live.Resume["title"] = cond
		if err := c.UpdateResumeContent(ctx, hash, live, []string{"resume.title"}); err != nil {
			t.Fatalf("%s: apply title: %v", hash, err)
		}
		t.Logf("%s: title applied+verified: %q", hash, want)
	}
}

// TestLive97VerifyRequestedTitles re-reads the builder states and asserts
// the titles on hh equal the requested ones (mandate verification).
func TestLive97VerifyRequestedTitles(t *testing.T) {
	c := live96Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	for hash, want := range live97RequestedTitles {
		live, err := c.ResumeContent(ctx, hash)
		if err != nil {
			t.Fatalf("%s: live content: %v", hash, err)
		}
		if got := titleTextOfBuilder(live.Resume["title"]); got != want {
			t.Errorf("%s: title on hh = %q, want %q", hash, got, want)
		} else {
			t.Logf("%s: title verified: %q", hash, got)
		}
	}
}
