package autoclone

// T35: the resume-improve command addresses BASE resumes (no lineage row).
// The browser runner resolves accounts through the lineage by default; the
// command pins the account explicitly via EditorOptions.AccountID, and the
// runner must honor that pin instead of the lineage lookup.

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A base hash (no lineage row) with a pinned account applies through the
// editor: cookies come from the pinned account, no lineage error.
func TestApplyFieldPlanPinnedAccountSkipsLineage(t *testing.T) {
	st := newFakeEditor(t)
	srv := mockEditorServer(t, st)
	defer srv.Close()

	factory := &fakeBrowserFactory{browser: &fakeBrowser{
		apply: applyOpsToState(t, &st.state, planByLocator(testPlan())),
	}}
	db := testStore(t) // NO seedClone: the hash has no lineage row
	r := editorTestRunner(t, editorCfg(srv.URL), db, st, factory, nil)
	r.opt.AccountID = "acc1"

	res, err := r.ApplyFieldPlan(context.Background(), "b4se123", testPlan())
	if err != nil {
		t.Fatalf("ApplyFieldPlan with pinned account: %v", err)
	}
	if len(res.Applied) != 4 {
		t.Fatalf("applied = %v (failed %+v)", res.Applied, res.Failed)
	}
}

// Without the pin the runner keeps the strict lineage invariant.
func TestApplyFieldPlanUnpinnedBaseHashIsNotClone(t *testing.T) {
	st := newFakeEditor(t)
	srv := mockEditorServer(t, st)
	defer srv.Close()

	factory := &fakeBrowserFactory{browser: &fakeBrowser{}}
	db := testStore(t)
	r := editorTestRunner(t, editorCfg(srv.URL), db, st, factory, nil)

	_, err := r.ApplyFieldPlan(context.Background(), "b4se123", testPlan())
	if !errors.Is(err, ErrNotClone) {
		t.Fatalf("err = %v, want ErrNotClone", err)
	}
}

// The pinned account also drives the cookie source (the session injected
// into the browser belongs to the pinned account).
func TestApplyFieldPlanPinnedAccountUsesItsCookies(t *testing.T) {
	st := newFakeEditor(t)
	srv := mockEditorServer(t, st)
	defer srv.Close()

	factory := &fakeBrowserFactory{browser: &fakeBrowser{}}
	db := testStore(t)
	src := &fakeCookieSource{byAccount: map[string][]*http.Cookie{
		"acc1": testSessionCookie(),
	}}
	r := editorTestRunner(t, editorCfg(srv.URL), db, st, factory, src)
	r.opt.AccountID = "acc1"

	if _, err := r.ApplyFieldPlan(context.Background(), "b4se123", testPlan()); err != nil {
		t.Fatalf("ApplyFieldPlan: %v", err)
	}
	if len(src.requested) != 1 || src.requested[0] != "acc1" {
		t.Fatalf("cookie accounts = %v, want [acc1]", src.requested)
	}
}
