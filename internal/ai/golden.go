package ai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/config"
)

// RunGolden executes every golden fixture under dir against the full Runner
// pipeline and compares the produced verdict with the expected one. It is
// the regression harness behind the future `hhauto ai eval` CLI command.
//
// Fixture layout (one directory per case, name = directory name):
//
//	input.json     {"task": "<TaskName>", "input": <any>}   (required)
//	raw.json       canned model reply, if it differs from the expected
//	               verdict (e.g. to exercise post-checks); otherwise the
//	               expected verdict is fed to the pipeline verbatim
//	expected.json  the final, schema-valid verdict after post-checks
func RunGolden(ctx context.Context, dir string, t *testing.T) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ai: golden: read %s: %v", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		caseDir := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(caseDir, "input.json")); err != nil {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			runGoldenCase(ctx, caseDir, t)
		})
	}
}

func runGoldenCase(ctx context.Context, caseDir string, t *testing.T) {
	t.Helper()
	rawInput, err := os.ReadFile(filepath.Join(caseDir, "input.json"))
	if err != nil {
		t.Fatalf("read input: %v", err)
	}
	var fixture struct {
		Task  string          `json:"task"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rawInput, &fixture); err != nil {
		t.Fatalf("decode input.json: %v", err)
	}
	task, ok := taskByName(fixture.Task)
	if !ok {
		t.Fatalf("unknown task %q", fixture.Task)
	}
	expected, err := os.ReadFile(filepath.Join(caseDir, "expected.json"))
	if err != nil {
		t.Fatalf("read expected.json: %v", err)
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
		t.Fatalf("New: %v", err)
	}
	verdict, _, err := r.Run(ctx, "golden-account", "golden", filepath.Base(caseDir), "golden-run", task, fixture.Input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if canon(t, expected) != canon(t, verdict) {
		t.Errorf("verdict mismatch\n want: %s\n got:  %s", canon(t, expected), canon(t, verdict))
	}
}

// canon normalizes JSON for order-insensitive comparison.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("canonicalize %s: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	return string(out)
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
