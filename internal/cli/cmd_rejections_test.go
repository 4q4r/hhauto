package cli

// T123: the rejections analyze command — fail-fast without the AI task.
// The happy path (collect → analyze → store → report) is covered in
// internal/features/rejections; this only pins the command wiring.

import (
	"strings"
	"testing"
)

func TestRejectionsAnalyzeCmdFailsFastWithoutAI(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	_, err := runRoot(t, "rejections", "analyze", "--config", cfgPath)
	if err == nil {
		t.Fatal("expected fail-fast without [ai.tasks.reject_analysis]")
	}
	if !strings.Contains(err.Error(), "reject_analysis") {
		t.Fatalf("err = %v, want a reject_analysis hint", err)
	}
}
