package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderProfileMD(t *testing.T) {
	qs := []string{"Вопрос 1?", "Вопрос 2?"}
	md := renderProfileMD(qs, []string{"Ответ 1", ""})
	if !strings.Contains(md, "## Вопрос 1?") || !strings.Contains(md, "Ответ 1") {
		t.Fatalf("md missing filled answer:\n%s", md)
	}
	if !strings.Contains(md, "<!-- ответ не заполнен -->") {
		t.Fatalf("md missing the unanswered marker:\n%s", md)
	}
}

func TestDefaultUARealistic(t *testing.T) {
	ua := defaultUA()
	if !strings.HasPrefix(ua, "Mozilla/5.0") || !strings.Contains(ua, "Chrome/") || !strings.Contains(ua, "Safari/") {
		t.Fatalf("default UA not a realistic Chrome UA: %s", ua)
	}
}

func TestValidAccountID(t *testing.T) {
	for _, ok := range []string{"main", "acc-1", "a_b.c"} {
		if !validAccountID(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "тест", "a b"} {
		if validAccountID(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

func TestAppendAndRemoveAccountTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hhauto.toml")
	base := "# created\n\n[general]\ndata_dir = \"" + filepath.ToSlash(dir) + "\"\n\n" +
		"[accounts.main]\nuser_agent = \"ua-main\"\n\n" +
		"[accounts.main.headers]\n\"Accept-Language\" = \"ru\"\n"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	// The existing config must load before an append (fail-fast guard).
	if err := appendAccountToTOML(path, "second", "Second", "", "ua-2"); err != nil {
		t.Fatalf("append rejected: %v", err)
	}
	if err := appendAccountToTOML(path, "second", "Second", "", "ua-2"); err == nil {
		t.Fatal("duplicate account append must fail")
	}

	// The appended section parses.
	block, err := accountTOMLBlock("third", "Third", "", "ua-3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(block, "[accounts.third]") {
		t.Fatalf("block header wrong: %s", block)
	}

	// Remove main including its sub-table, keep the rest.
	removed, err := removeAccountSection(path, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("main section not removed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "ua-main") || strings.Contains(text, "[accounts.main.headers]") {
		t.Fatalf("main section remnants:\n%s", text)
	}
	if !strings.Contains(text, "[accounts.second]") || strings.Contains(text, "[accounts.second].") {
		t.Fatalf("second account damaged:\n%s", text)
	}
	// A missing section reports removed=false.
	if removed2, err := removeAccountSection(path, "ghost"); err != nil || removed2 {
		t.Fatalf("ghost removal: removed=%v err=%v", removed2, err)
	}
}

func TestEnsureConfigFileCreatesSkeleton(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hhauto.toml")
	created, err := ensureConfigFile(path)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	created2, err := ensureConfigFile(path)
	if err != nil || created2 {
		t.Fatalf("second call created=%v err=%v", created2, err)
	}
}
