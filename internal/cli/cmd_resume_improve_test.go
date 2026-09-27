package cli

// T35 `hhauto resume improve` unit tests: the flag parsing, the target
// resolution (clone lineage → base snapshots → explicit --account) and the
// run task-type mapping. The end-to-end invariant (base resumes need
// --confirm) rides these helpers; the pipeline itself is covered in
// internal/features/autoclone.

import (
	"context"
	"errors"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/store"
	"hhauto/internal/tasklock"
)

func TestParseFieldsFlagDefaults(t *testing.T) {
	got, err := parseFieldsFlag("", []string{"title", "about"})
	if err != nil {
		t.Fatalf("parseFieldsFlag: %v", err)
	}
	if len(got) != 2 || got[0] != "title" || got[1] != "about" {
		t.Fatalf("got %v, want config defaults", got)
	}
}

func TestParseFieldsFlagSubset(t *testing.T) {
	got, err := parseFieldsFlag(" title, salary ,about", nil)
	if err != nil {
		t.Fatalf("parseFieldsFlag: %v", err)
	}
	if len(got) != 3 || got[0] != "title" || got[1] != "salary" || got[2] != "about" {
		t.Fatalf("got %v", got)
	}
}

func TestParseFieldsFlagUnknownField(t *testing.T) {
	if _, err := parseFieldsFlag("title,not_a_field", nil); err == nil {
		t.Fatal("unknown field must fail")
	}
}

func TestParseFieldsFlagEmptyToken(t *testing.T) {
	if _, err := parseFieldsFlag("title,,about", nil); err == nil {
		t.Fatal("empty token must fail")
	}
}

func TestResolveImproveTargetCloneLineage(t *testing.T) {
	db := cliTestStore(t)
	if err := db.ResumeClonesRegister(context.Background(), &store.ResumeClone{
		CloneHash: "cl0ne1", ParentHash: "p4rent", Depth: 1, AccountID: "main",
	}); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{"main": {}}}, DB: db}
	accountID, isClone, err := resolveImproveTarget(context.Background(), rt, "cl0ne1", "")
	if err != nil {
		t.Fatalf("resolveImproveTarget: %v", err)
	}
	if accountID != "main" || !isClone {
		t.Fatalf("account=%q isClone=%v, want main/true", accountID, isClone)
	}
}

func TestResolveImproveTargetBaseSnapshot(t *testing.T) {
	db := cliTestStore(t)
	if err := db.AccountsUpsert(context.Background(), store.Account{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ResumesUpsert(context.Background(), &store.Resume{
		AccountID: "main", HhResumeID: "b4se1", Title: "Базовое",
	}); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{"main": {}}}, DB: db}
	accountID, isClone, err := resolveImproveTarget(context.Background(), rt, "b4se1", "")
	if err != nil {
		t.Fatalf("resolveImproveTarget: %v", err)
	}
	if accountID != "main" || isClone {
		t.Fatalf("account=%q isClone=%v, want main/false", accountID, isClone)
	}
}

func TestResolveImproveTargetAccountFlagWins(t *testing.T) {
	db := cliTestStore(t)
	if err := db.ResumeClonesRegister(context.Background(), &store.ResumeClone{
		CloneHash: "cl0ne1", ParentHash: "p4rent", Depth: 1, AccountID: "other",
	}); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{
		"main": {}, "other": {},
	}}, DB: db}
	accountID, isClone, err := resolveImproveTarget(context.Background(), rt, "cl0ne1", "other")
	if err != nil {
		t.Fatalf("resolveImproveTarget: %v", err)
	}
	if accountID != "other" || !isClone {
		t.Fatalf("account=%q isClone=%v", accountID, isClone)
	}
	// A lineage row bound to a DIFFERENT account must be rejected.
	if _, _, err := resolveImproveTarget(context.Background(), rt, "cl0ne1", "main"); err == nil {
		t.Fatal("account mismatch must fail")
	}
}

func TestResolveImproveTargetNotFound(t *testing.T) {
	db := cliTestStore(t)
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{"main": {}}}, DB: db}
	_, _, err := resolveImproveTarget(context.Background(), rt, "ghost", "")
	if err == nil {
		t.Fatal("unknown hash must fail with an --account hint")
	}
}

func TestRunTaskTypeFeatureMapping(t *testing.T) {
	feat, runnable := runTaskTypeFeature(tasklock.TypeAll)
	if !runnable || feat != "" {
		t.Fatalf("all: (%q,%v)", feat, runnable)
	}
	feat, runnable = runTaskTypeFeature(tasklock.TypeMultiApply)
	if !runnable || feat != "multiapply" {
		t.Fatalf("multiapply: (%q,%v)", feat, runnable)
	}
	if _, runnable := runTaskTypeFeature(tasklock.TypeResumeImprove); runnable {
		t.Fatal("resume_improve must not map to recurring engine workers")
	}
}

func TestNormalizeTaskType(t *testing.T) {
	if tt, err := normalizeTaskType(""); err != nil || tt != tasklock.TypeAll {
		t.Fatalf("empty: %q %v", tt, err)
	}
	if tt, err := normalizeTaskType("multiapply"); err != nil || tt != tasklock.TypeMultiApply {
		t.Fatalf("multiapply: %q %v", tt, err)
	}
	if _, err := normalizeTaskType("bogus"); !errors.Is(err, errUnknownTaskType) {
		t.Fatalf("bogus: %v", err)
	}
}
