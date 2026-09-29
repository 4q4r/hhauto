package cli

// T54 E2E: the human resolution flow over the parked needs_human verdicts
// through the real cobra tree — list/show render the parked было/стало,
// `resolve --decision apply` pushes the value through the builder-update
// API onto the fake hh, `--value` overrides the AI proposal with the
// human's text, `discard` closes without applying, and a resolved item
// refuses a second resolve.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"hhauto/internal/features/autoclone"
)

// t54ResolveExtra pins the builder-update endpoint onto the fake hh site
// (production defaults to the absolute resume-profile-front URL). The AI
// wiring is deliberately absent: resolving never calls the AI.
func t54ResolveExtra() string {
	return `
[api.endpoints.override]
resume_update = "` + testSite + `/profile/shards/resume/update"
`
}

// seedNeedHumanItem parks one open needs_human verdict for the fake hh's
// resume b4se1 and returns its id.
func seedNeedHumanItem(t *testing.T, cfgPath, after string) int64 {
	t.Helper()
	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	payload := `{"clone":"b4se1","field":"title","locator":"resume.title","item":-1,` +
		`"before":"DevOps-инженер","after":` + after + `}`
	id, err := rt.DB.ReviewItemsAdd(newContext(), "main", autoclone.ReviewKindImproveNeedsHuman,
		"improve verdict needs human: number_not_in_sources", payload)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func reviewItemByID(t *testing.T, cfgPath string, id int64) (status, resolution string) {
	t.Helper()
	rt, err := Build(newContext(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	it, err := rt.DB.ReviewItemsGet(newContext(), id)
	if err != nil {
		t.Fatal(err)
	}
	if it == nil {
		t.Fatalf("item %d vanished", id)
	}
	if it.Resolution != nil {
		return it.Status, *it.Resolution
	}
	return it.Status, ""
}

// apply applies the AI proposal as-is: the value lands on the resume and
// the item closes with the human apply audit trail.
func TestReviewResolveApplyE2E(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "apply", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resolve apply: %v\n%s", err, out)
	}
	if got := st.title(); got != "DevOps-инженер 2026" {
		t.Fatalf("resume title after apply = %q", got)
	}
	status, resolution := reviewItemByID(t, cfgPath, id)
	if status != "resolved" {
		t.Fatalf("status = %q", status)
	}
	if !strings.Contains(resolution, "human: apply") {
		t.Fatalf("resolution = %q", resolution)
	}
}

// apply --value applies the HUMAN's text instead of the AI proposal.
func TestReviewResolveApplyOwnerValueE2E(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "apply",
		"--value", "DevOps-инженер (правка владельца)", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resolve apply --value: %v\n%s", err, out)
	}
	if got := st.title(); got != "DevOps-инженер (правка владельца)" {
		t.Fatalf("resume title after apply --value = %q", got)
	}
	status, resolution := reviewItemByID(t, cfgPath, id)
	if status != "resolved" || !strings.Contains(resolution, "human: apply") {
		t.Fatalf("status/resolution = %q/%q", status, resolution)
	}
}

// discard closes the item without touching the resume.
func TestReviewResolveDiscardE2E(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resolve discard: %v\n%s", err, out)
	}
	if got := st.title(); got != "DevOps-инженер" {
		t.Fatalf("discard mutated the resume: %q", got)
	}
	status, resolution := reviewItemByID(t, cfgPath, id)
	if status != "resolved" || !strings.Contains(resolution, "human: discard") {
		t.Fatalf("status/resolution = %q/%q", status, resolution)
	}
}

// A resolved item refuses a second resolve with a clear error.
func TestReviewResolveRepeatIsError(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	if _, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard", "--config", cfgPath); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	_, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "уже закрыто") {
		t.Fatalf("second resolve: err = %v, want уже закрыто", err)
	}
	if got := st.title(); got != "DevOps-инженер" {
		t.Fatalf("resume mutated on repeat: %q", got)
	}
}

// apply without a stored proposal and without --value is a clear error.
func TestReviewResolveApplyWithoutValueErrors(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `null`)

	_, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "apply", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "--value") {
		t.Fatalf("apply without a value: err = %v", err)
	}
	if got := st.title(); got != "DevOps-инженер" {
		t.Fatalf("resume mutated: %q", got)
	}
	status, _ := reviewItemByID(t, cfgPath, id)
	if status != "open" {
		t.Fatalf("failed apply resolved the item: %q", status)
	}
}

// --value paired with discard is a contradiction: error, item stays open.
func TestReviewResolveValueWithDiscardErrors(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `null`)

	_, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard",
		"--value", "зачем-то передали", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "--value") {
		t.Fatalf("--value with discard: err = %v", err)
	}
	status, _ := reviewItemByID(t, cfgPath, id)
	if status != "open" {
		t.Fatalf("item closed: %q", status)
	}
}

// [general] dry_run forces the same plan-only behavior as the flag; the
// fixed test config already defines [general], so the flag path below is
// the tested proxy for the same boolean.
func TestReviewResolveApplyDryRunE2E(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "apply", "--dry-run", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resolve apply --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run not announced:\n%s", out)
	}
	if got := st.title(); got != "DevOps-инженер" {
		t.Fatalf("dry-run mutated the resume: %q", got)
	}
	if status, _ := reviewItemByID(t, cfgPath, id); status != "open" {
		t.Fatalf("dry-run resolved the item: %q", status)
	}
}

// --json renders the resolution result machine-readably.
func TestReviewResolveJSONOutput(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "resolve", fmt.Sprint(id), "--decision", "discard", "--json", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resolve --json: %v\n%s", err, out)
	}
	var res struct {
		ID       int64  `json:"id"`
		Decision string `json:"decision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out[strings.Index(out, "{"):])), &res); err != nil {
		t.Fatalf("json output: %v\n%s", err, out)
	}
	if res.ID != id || res.Decision != "discard" || res.Status != "resolved" {
		t.Fatalf("json result = %+v", res)
	}
}

// list renders the parked verdict compactly: resume hash and field from
// the payload alongside the usual columns.
func TestReviewListShowsNeedsHumanColumns(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "list", "--config", cfgPath)
	if err != nil {
		t.Fatalf("review list: %v", err)
	}
	for _, want := range []string{fmt.Sprint(id), "b4se1", "title", "number_not_in_sources"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
}

// show renders the full было/стало diff of the parked verdict.
func TestReviewShowPrintsDiff(t *testing.T) {
	st := &improveHHState{Title: "DevOps-инженер"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t54ResolveExtra())
	id := seedNeedHumanItem(t, cfgPath, `"DevOps-инженер 2026"`)

	out, err := runRoot(t, "review", "show", fmt.Sprint(id), "--config", cfgPath)
	if err != nil {
		t.Fatalf("review show: %v", err)
	}
	for _, want := range []string{"Было", "Стало", "DevOps-инженер", "DevOps-инженер 2026", "number_not_in_sources"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
}
