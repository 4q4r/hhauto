package autolift

import (
	"context"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/store"
)

// T26: the autoclone autolift gate — clones are regular resumes and lift by
// the same 4h store timer unless [features.autoclone] autolift=false
// excludes the tracked clone pool.

func boolPtrA(v bool) *bool { return &v }

func TestCloneAutoliftGate(t *testing.T) {
	fresh := func() []fakeResume {
		return []fakeResume{
			{Hash: "hash1", Title: "Go dev", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)},
			{Hash: "clone1", Title: "Go dev (clone)", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)},
		}
	}

	t.Run("default true lifts clones", func(t *testing.T) {
		h := newHarness(t, nil, fresh()...)
		if err := h.db.ResumeClonesRegister(context.Background(), &store.ResumeClone{
			CloneHash: "clone1", ParentHash: "hash1", Depth: 1, AccountID: "acc1",
		}); err != nil {
			t.Fatalf("register lineage: %v", err)
		}
		f := h.feature(t)
		if err := f.RunAccount(context.Background(), "acc1"); err != nil {
			t.Fatalf("RunAccount: %v", err)
		}
		if got := h.fake.touch; got != 2 {
			t.Fatalf("touch calls = %d, want 2 (base + clone)", got)
		}
	})

	t.Run("autolift=false skips tracked clones", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.Features.AutoClone = config.AutoClone{Autolift: boolPtrA(false)}
		}, fresh()...)
		if err := h.db.ResumeClonesRegister(context.Background(), &store.ResumeClone{
			CloneHash: "clone1", ParentHash: "hash1", Depth: 1, AccountID: "acc1",
		}); err != nil {
			t.Fatalf("register lineage: %v", err)
		}
		f := h.feature(t)
		if err := f.RunAccount(context.Background(), "acc1"); err != nil {
			t.Fatalf("RunAccount: %v", err)
		}
		if got := h.fake.touch; got != 1 {
			t.Fatalf("touch calls = %d, want 1 (base resume only)", got)
		}
	})

	t.Run("untracked clones still lift with gate off", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			c.Features.AutoClone = config.AutoClone{Autolift: boolPtrA(false)}
		}, fresh()...)
		f := h.feature(t)
		if err := f.RunAccount(context.Background(), "acc1"); err != nil {
			t.Fatalf("RunAccount: %v", err)
		}
		if got := h.fake.touch; got != 2 {
			t.Fatalf("touch calls = %d, want 2 (no lineage rows: nothing to exclude)", got)
		}
	})
}
