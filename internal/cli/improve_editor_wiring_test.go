package cli

// T30 wiring tests: the browser editor runner rides the improver when
// [features.autoclone.improve.editor] is enabled, the per-account content
// surface resolves through the local lineage (hh clients are
// per-account), and the cookie store is adapted onto the runner's
// EditorCookieSource without ever leaking the session outside hh.ru.
import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// stubContentEditor implements the content-editor surface only (the
// lineageEditor dispatches by account and never needs the full HHAPI).
type stubContentEditor struct {
	lastHash string
}

func (s *stubContentEditor) ResumeContent(_ context.Context, hash string) (*hhapi.ResumeContent, error) {
	s.lastHash = hash
	return nil, nil
}

func (s *stubContentEditor) UpdateResumeContent(context.Context, string, *hhapi.ResumeContent, []string) error {
	return nil
}

func cliTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestLineageEditorResolvesPerAccount: the clone hash maps through the
// lineage to the owning account's content editor.
func TestLineageEditorResolvesPerAccount(t *testing.T) {
	db := cliTestStore(t)
	if err := db.ResumeClonesRegister(context.Background(), &store.ResumeClone{
		CloneHash: "clone1", ParentHash: "parent1", Depth: 1, AccountID: "acc1",
	}); err != nil {
		t.Fatalf("seed clone: %v", err)
	}
	ed := &stubContentEditor{}
	called := ""
	le := &lineageEditor{db: db, apiFor: func(accountID string) (contentEditorAPI, error) {
		called = accountID
		return ed, nil
	}}
	if _, err := le.ResumeContent(context.Background(), "clone1"); err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	if called != "acc1" {
		t.Fatalf("api resolved for account %q, want acc1", called)
	}
	if ed.lastHash != "clone1" {
		t.Fatalf("editor got hash %q, want clone1", ed.lastHash)
	}

	// An unknown hash is refused (the base-resume invariant holds at the
	// wiring layer too).
	if _, err := le.ResumeContent(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown hash must be refused")
	}

	// An apiFor failure surfaces verbatim.
	le.apiFor = func(string) (contentEditorAPI, error) { return nil, errors.New("no session") }
	if _, err := le.ResumeContent(context.Background(), "clone1"); err == nil {
		t.Fatal("apiFor failure must surface")
	}
}

// TestBuildImproveEditorWiring: editor enabled constructs the improver
// with the browser runner; cookie-store absence fails fast.
func TestBuildImproveEditorWiring(t *testing.T) {
	cfg := improveWiringCfg()
	cfg.AI.Tasks = map[string]config.AITask{}
	injectImproveTask(cfg)
	cfg.Features.AutoClone.Improve.Editor.Enabled = true

	db := cliTestStore(t)
	stub := &fieldPromptRunnerStubT29{}
	apiFor := func(string) (contentEditorAPI, error) { return &stubContentEditor{}, nil }

	// No cookie store + editor enabled → fail fast at wiring.
	if _, err := buildImprove(cfg, stub, db, apiFor, nil, testLoggerT29()); err == nil {
		t.Fatal("editor.enabled without a cookie store must fail at wiring")
	}

	// Fully wired: construction succeeds (Chrome absence degrades at
	// apply time, not at startup).
	imp, err := buildImprove(cfg, stub, db, apiFor, stubCookieStoreT30{}, testLoggerT29())
	if err != nil {
		t.Fatalf("buildImprove: %v", err)
	}
	if imp == nil {
		t.Fatal("improver must be constructed")
	}
}

// stubCookieStoreT30 is a cookies.Store stand-in for the wiring test.
type stubCookieStoreT30 struct{}

func (stubCookieStoreT30) Load(context.Context, string) ([]*http.Cookie, error) {
	return []*http.Cookie{{Name: "_xsrf", Value: "tok", Domain: ".hh.ru"}}, nil
}

func (stubCookieStoreT30) Save(context.Context, string, []*http.Cookie) error { return nil }
func (stubCookieStoreT30) Delete(context.Context, string) error               { return nil }
func (stubCookieStoreT30) Backend() string                                    { return "stub" }
