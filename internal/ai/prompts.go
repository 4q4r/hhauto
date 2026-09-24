package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"hhauto/internal/config"
)

// promptFile is one builtin prompt parsed from internal/aiprompts/*.md.
type promptFile struct {
	ID      string `toml:"id"`
	Version int    `toml:"version"`
	Task    string `toml:"task"`
}

type builtinPrompt struct {
	body    string
	version int
}

// PromptRegistry resolves per-task system prompts: TOML override
// (inline | file, never both) wins over the builtin embedded default.
// It is immutable after construction and safe for concurrent use.
type PromptRegistry struct {
	builtin map[TaskName]builtinPrompt
	body    map[TaskName]string
	ver     map[TaskName]string
}

// NewPromptRegistry loads the builtin prompts from fsys and resolves the
// configured overrides. tasks maps task names to their [ai.tasks.<task>]
// config; entries may be empty (builtin default is then used). Unknown task
// names, both-prompt overrides and unreadable override files are config
// errors (fail fast, no silent fallback).
func NewPromptRegistry(tasks map[string]config.AITask, fsys fs.FS) (*PromptRegistry, error) {
	r := &PromptRegistry{
		builtin: map[TaskName]builtinPrompt{},
		body:    map[TaskName]string{},
		ver:     map[TaskName]string{},
	}
	if err := loadBuiltinPrompts(r, fsys); err != nil {
		return nil, err
	}
	if err := requireBuiltinCoverage(r); err != nil {
		return nil, err
	}
	if err := resolveOverrides(r, tasks); err != nil {
		return nil, err
	}
	return r, nil
}

// loadBuiltinPrompts reads every *.md file in fsys and registers it as the
// builtin prompt for its front-matter task.
func loadBuiltinPrompts(r *PromptRegistry, fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("ai: prompts: read embedded fs: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return fmt.Errorf("ai: prompts: read %s: %w", e.Name(), err)
		}
		meta, body, err := parsePromptFrontMatter(string(raw))
		if err != nil {
			return fmt.Errorf("ai: prompts: %s: %w", e.Name(), err)
		}
		task := TaskName(meta.Task)
		if _, ok := taskByName(string(task)); !ok {
			return fmt.Errorf("ai: prompts: %s: unknown task %q", e.Name(), meta.Task)
		}
		if _, dup := r.builtin[task]; dup {
			return fmt.Errorf("ai: prompts: duplicate builtin for task %q", task)
		}
		if meta.Version < 1 {
			return fmt.Errorf("ai: prompts: %s: version must be >= 1", e.Name())
		}
		if strings.TrimSpace(body) == "" {
			return fmt.Errorf("ai: prompts: %s: empty body", e.Name())
		}
		r.builtin[task] = builtinPrompt{body: body, version: meta.Version}
	}
	return nil
}

// requireBuiltinCoverage fails when a task has no builtin prompt.
func requireBuiltinCoverage(r *PromptRegistry) error {
	for name := range taskNames {
		if _, ok := r.builtin[name]; !ok {
			return fmt.Errorf("ai: prompts: no builtin prompt for task %q", name)
		}
	}
	return nil
}

// resolveOverrides applies the per-task prompt overrides from config.
func resolveOverrides(r *PromptRegistry, tasks map[string]config.AITask) error {
	for name, cfg := range tasks {
		task := TaskName(name)
		if _, ok := taskByName(name); !ok {
			return fmt.Errorf("ai: prompts: ai.tasks[%q]: unknown task", name)
		}
		if cfg.PromptInline != "" && cfg.PromptFile != "" {
			return fmt.Errorf("ai: prompts: ai.tasks[%q]: inline and file are mutually exclusive", name)
		}
		body := r.builtin[task].body
		ver := fmt.Sprintf("v%d", r.builtin[task].version)
		switch {
		case cfg.PromptInline != "":
			body = cfg.PromptInline
			ver = customVersion(cfg.PromptVersion, body)
		case cfg.PromptFile != "":
			raw, err := os.ReadFile(cfg.PromptFile)
			if err != nil {
				return fmt.Errorf("ai: prompts: ai.tasks[%q]: %w", name, err)
			}
			body = string(raw)
			ver = customVersion(cfg.PromptVersion, body)
		}
		r.body[task] = body
		r.ver[task] = ver
	}
	return nil
}

// customVersion returns the explicit version or a content hash so cache keys
// and ledger records change when an unversioned override is edited.
func customVersion(explicit, body string) string {
	if explicit != "" {
		return explicit
	}
	sum := sha256.Sum256([]byte(body))
	return "custom-" + hex.EncodeToString(sum[:4])
}

// SystemPrompt returns the resolved body and version string for the task.
func (r *PromptRegistry) SystemPrompt(task TaskName) (body, version string, err error) {
	body, ok := r.body[task]
	if !ok {
		return "", "", fmt.Errorf("ai: prompts: task %q is not configured in [ai.tasks]", task)
	}
	return body, r.ver[task], nil
}

// parsePromptFrontMatter splits TOML front-matter from the Markdown body.
func parsePromptFrontMatter(src string) (promptFile, string, error) {
	var meta promptFile
	const fence = "---"
	rest := strings.TrimPrefix(src, "\ufeff")
	if !strings.HasPrefix(rest, fence+"\n") {
		return meta, "", fmt.Errorf("missing front-matter")
	}
	rest = rest[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return meta, "", fmt.Errorf("unterminated front-matter")
	}
	head, body := rest[:end], rest[end+len(fence)+1:]
	if err := toml.Unmarshal([]byte(head), &meta); err != nil {
		return meta, "", fmt.Errorf("front-matter: %w", err)
	}
	if meta.Task == "" {
		meta.Task = meta.ID
	}
	return meta, strings.TrimSpace(body), nil
}
