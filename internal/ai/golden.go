package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"hhauto/internal/config"
)

// GoldenResult is one fixture outcome: Err is nil when the pipeline verdict
// matched the expectation.
type GoldenResult struct {
	Name string
	Want string
	Got  string
	Err  error
}

// RunGolden executes every golden fixture under dir against the full Runner
// pipeline and reports per-case results. It is the regression harness behind
// the future `hhauto ai eval` CLI command; assertions live with the caller
// (see golden_test.go).
//
// Fixture layout (one directory per case, name = directory name):
//
//	input.json     {"task": "<TaskName>", "input": <any>}   (required)
//	raw.json       canned model reply, if it differs from the expected
//	               verdict (e.g. to exercise post-checks); otherwise the
//	               expected verdict is fed to the pipeline verbatim
//	expected.json  the final, schema-valid verdict after post-checks
func RunGolden(ctx context.Context, dir string) ([]GoldenResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ai: golden: read %s: %w", dir, err)
	}
	var results []GoldenResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		caseDir := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(caseDir, "input.json")); err != nil {
			continue
		}
		res := GoldenResult{Name: e.Name()}
		res.Err = runGoldenCase(ctx, caseDir, &res.Want, &res.Got)
		results = append(results, res)
	}
	return results, nil
}

func runGoldenCase(ctx context.Context, caseDir string, want, got *string) error {
	rawInput, err := os.ReadFile(filepath.Join(caseDir, "input.json"))
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	var fixture struct {
		Task  string          `json:"task"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rawInput, &fixture); err != nil {
		return fmt.Errorf("decode input.json: %w", err)
	}
	task, ok := taskByName(fixture.Task)
	if !ok {
		return fmt.Errorf("unknown task %q", fixture.Task)
	}
	expected, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
	if err != nil {
		return fmt.Errorf("read expected.json: %w", err)
	}
	rawReply := expected
	if rawBytes, err := os.ReadFile(filepath.Join(caseDir, "raw.json")); err == nil {
		rawReply = rawBytes
	}

	gp := &goldenProvider{content: string(rawReply)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := New(
		map[string]Provider{"golden": gp},
		map[string]config.AIModel{"profile": {Provider: "golden", Model: "golden-model"}},
		map[string]config.AITask{string(task): {ModelProfile: "profile"}},
		nil, nil, log)
	if err != nil {
		return fmt.Errorf("New: %w", err)
	}
	verdict, _, err := r.Run(ctx, "golden-account", "golden", filepath.Base(caseDir), "golden-run", task, fixture.Input)
	if err != nil {
		return fmt.Errorf("Run: %w", err)
	}
	wantS, err := canonical(expected)
	if err != nil {
		return fmt.Errorf("canonicalize expected: %w", err)
	}
	gotS, err := canonical(verdict)
	if err != nil {
		return fmt.Errorf("canonicalize verdict: %w", err)
	}
	*want, *got = wantS, gotS
	if wantS != gotS {
		return fmt.Errorf("verdict mismatch")
	}
	return nil
}

// canonical normalizes JSON for order-insensitive comparison.
func canonical(raw []byte) (string, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// goldenProvider is a scripted provider used only by golden fixtures; it
// always returns its canned content and never talks to the network.
type goldenProvider struct{ content string }

func (g *goldenProvider) Complete(_ context.Context, _ Request) (Response, error) {
	return Response{Content: g.content, Model: "golden-model", TokensIn: 0, TokensOut: 0}, nil
}

func (g *goldenProvider) Dialect() string { return "golden" }

func (g *goldenProvider) completeModel(ctx context.Context, model string, req Request) (Response, error) {
	resp, err := g.Complete(ctx, req)
	resp.Model = model
	return resp, err
}
