package multiapply

// T29 hook tests: after a verified clone is registered, the improver runs
// against the CLONE hash and the configured clone_original_action applies
// to the ORIGINAL (keep default = no management POSTs; hide/delete ride
// the resume-management endpoints; a fatal improve error keeps the
// original).
import (
	"context"
	"io"
	"net/http"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/features/autoclone"
)

type stubImprover struct {
	calls  []stubImproveCall
	errAll bool
}

type stubImproveCall struct {
	account, parent, clone string
}

func (s *stubImprover) ImproveClone(_ context.Context, accountID, parentHash, cloneHash string) (*autoclone.DiffArtifact, error) {
	s.calls = append(s.calls, stubImproveCall{accountID, parentHash, cloneHash})
	if s.errAll {
		return nil, context.DeadlineExceeded
	}
	return &autoclone.DiffArtifact{CloneHash: cloneHash}, nil
}

func autocloneCfg() func(*config.Config) {
	return func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	}
}

// The full clone path with the improver wired: ImproveClone sees the
// (account, parent, clone) triple; the run proceeds.
func TestAutoCloneImproveHookRunsOnClone(t *testing.T) {
	h := newHarness(t, autocloneCfg())
	twoVacancies(h)
	imp := &stubImprover{}
	f := buildCloneFeature(t, h, func(o *Options) { o.Improver = imp })

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if len(imp.calls) != 1 {
		t.Fatalf("improve calls = %d, want 1", len(imp.calls))
	}
	call := imp.calls[0]
	if call.parent != "hash1" || call.clone == "" || call.clone == "hash1" {
		t.Fatalf("improve call = %+v (must target the fresh clone)", call)
	}
	if h.hidePosts+h.deletePosts != 0 {
		t.Fatal("default keep must not post management calls")
	}
}

// clone_original_action=hide: after the clone (and improve) the ORIGINAL
// resume is hidden through the visibility endpoint.
func TestAutoCloneOriginActionHide(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		autocloneCfg()(c)
		c.Features.AutoClone.CloneOriginalAction = config.CloneActionHide
	})
	twoVacancies(h)
	imp := &stubImprover{}
	f := buildCloneFeature(t, h, func(o *Options) { o.Improver = imp })

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if h.hidePosts != 1 {
		t.Fatalf("hide posts = %d, want 1", h.hidePosts)
	}
	if len(h.hideBodies) != 1 || h.hideBodies[0] != "hash1" {
		t.Fatalf("hidden hashes = %v (must target the ORIGINAL hash1)", h.hideBodies)
	}
	if h.deletePosts != 0 {
		t.Fatal("hide must never delete")
	}
}

// clone_original_action=delete removes the original through the delete
// endpoint.
func TestAutoCloneOriginActionDelete(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		autocloneCfg()(c)
		c.Features.AutoClone.CloneOriginalAction = config.CloneActionDelete
	})
	twoVacancies(h)
	imp := &stubImprover{}
	f := buildCloneFeature(t, h, func(o *Options) { o.Improver = imp })

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if h.deletePosts != 1 {
		t.Fatalf("delete posts = %d, want 1", h.deletePosts)
	}
	if len(h.deleteBodies) != 1 || h.deleteBodies[0] != "hash1" {
		t.Fatalf("deleted hashes = %v (must target the ORIGINAL hash1)", h.deleteBodies)
	}
}

// A fatal improve error keeps the original (the action rides on
// clone+improve success).
func TestAutoCloneOriginActionKeptOnImproveFailure(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		autocloneCfg()(c)
		c.Features.AutoClone.CloneOriginalAction = config.CloneActionHide
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, func(o *Options) { o.Improver = &stubImprover{errAll: true} })

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if h.hidePosts != 0 {
		t.Fatal("a fatal improve error must keep the original")
	}
}

var (
	_ = io.Discard
	_ = http.NotFound
)
