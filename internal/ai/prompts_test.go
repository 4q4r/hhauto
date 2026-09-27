package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/aiprompts"
	"hhauto/internal/config"
)

func allTasks() map[string]config.AITask {
	tasks := map[string]config.AITask{}
	for _, name := range []string{
		"vacancy_scoring", "cover_letter", "chat_reply", "test_answer",
		"warmup_ping", "message_classification", "contact_extraction", "link_detection",
	} {
		tasks[name] = config.AITask{}
	}
	return tasks
}

func TestBuiltinPromptsLoadAllTasks(t *testing.T) {
	reg, err := NewPromptRegistry(allTasks(), aiprompts.FS)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	for _, task := range []TaskName{
		TaskVacancyScoring, TaskCoverLetter, TaskChatReply, TaskTestAnswer,
		TaskWarmupPing, TaskClassification, TaskContactExtract, TaskLinkDetection,
	} {
		body, ver, err := reg.SystemPrompt(task)
		if err != nil {
			t.Fatalf("%s: %v", task, err)
		}
		if len(body) < 200 {
			t.Errorf("%s: prompt body too short (%d bytes)", task, len(body))
		}
		if !strings.Contains(body, "DATA PROTECTION") {
			t.Errorf("%s: prompt lacks DATA PROTECTION section", task)
		}
		if !strings.Contains(body, "JSON") {
			t.Errorf("%s: prompt does not mention JSON output", task)
		}
		if ver == "" {
			t.Errorf("%s: empty prompt version", task)
		}
	}
}

func TestPromptInlineOverrideWins(t *testing.T) {
	tasks := allTasks()
	tasks["warmup_ping"] = config.AITask{PromptInline: "Ты генератор пингов. Отвечай JSON."}
	reg, err := NewPromptRegistry(tasks, aiprompts.FS)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	body, ver, err := reg.SystemPrompt(TaskWarmupPing)
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}
	if body != "Ты генератор пингов. Отвечай JSON." {
		t.Errorf("override body not used: %q", body)
	}
	if !strings.HasPrefix(ver, "custom-") {
		t.Errorf("override without explicit version should hash to custom-*, got %q", ver)
	}
}

func TestPromptOverrideExplicitVersionRespected(t *testing.T) {
	tasks := allTasks()
	tasks["warmup_ping"] = config.AITask{PromptInline: "Ты генератор пингов.", PromptVersion: "ab12"}
	reg, _ := NewPromptRegistry(tasks, aiprompts.FS)
	_, ver, err := reg.SystemPrompt(TaskWarmupPing)
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}
	if ver != "ab12" {
		t.Errorf("explicit version lost: %q", ver)
	}
}

func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("Файловый промпт."), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := allTasks()
	tasks["warmup_ping"] = config.AITask{PromptFile: path}
	reg, err := NewPromptRegistry(tasks, aiprompts.FS)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	body, _, err := reg.SystemPrompt(TaskWarmupPing)
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}
	if body != "Файловый промпт." {
		t.Errorf("file override not used: %q", body)
	}
}

func TestPromptBothInlineAndFileIsConfigError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := allTasks()
	tasks["warmup_ping"] = config.AITask{PromptInline: "a", PromptFile: path}
	if _, err := NewPromptRegistry(tasks, aiprompts.FS); err == nil {
		t.Fatal("expected error when both inline and file are set")
	}
}

func TestPromptMissingFileIsError(t *testing.T) {
	tasks := allTasks()
	tasks["warmup_ping"] = config.AITask{PromptFile: filepath.Join(t.TempDir(), "nope.md")}
	if _, err := NewPromptRegistry(tasks, aiprompts.FS); err == nil {
		t.Fatal("expected error for missing override file")
	}
}

func TestPromptUnknownTaskIsError(t *testing.T) {
	tasks := allTasks()
	tasks["bogus_task"] = config.AITask{}
	if _, err := NewPromptRegistry(tasks, aiprompts.FS); err == nil {
		t.Fatal("expected error for unknown task name")
	}
}

func TestPromptUnconfiguredTaskIsError(t *testing.T) {
	reg, err := NewPromptRegistry(map[string]config.AITask{}, aiprompts.FS)
	if err != nil {
		t.Fatalf("NewPromptRegistry with no tasks should still load builtins: %v", err)
	}
	if _, _, err := reg.SystemPrompt(TaskWarmupPing); err == nil {
		t.Fatal("expected error for unconfigured task")
	}
}
