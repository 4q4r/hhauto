package cli

// T53 style unit tests: the TTY/plain gate, the status vocabulary and the
// diff-block/table renderers. Non-TTY invariants (plain bytes) guard the
// T49 contract: pipes, journald and docker logs must not change.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"hhauto/internal/features/autoclone"
)

func TestStyleDisabledPassesThrough(t *testing.T) {
	var s style
	if got := s.green("x") + s.status("improved"); got != "ximproved" {
		t.Errorf("zero style must render plain, got %q", got)
	}
	if got := detectStyle(&bytes.Buffer{}); got.enabled {
		t.Error("non-file sink must disable colors")
	}
}

func TestNewStyleHonorsNoColorFlag(t *testing.T) {
	flags.noColor = true
	defer func() { flags.noColor = false }()
	if s := newStyle(true); s.enabled {
		t.Error("--no-color must disable colors")
	}
}

func TestColorEnvDisabled(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if !colorEnvDisabled() {
		t.Error("NO_COLOR must force plain output")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if !colorEnvDisabled() {
		t.Error("TERM=dumb must force plain output")
	}
}

func TestStyleEnabledContainsANSI(t *testing.T) {
	s := newStyle(true)
	for name, tc := range map[string]struct {
		got  string
		code string
	}{
		"red":   {got: s.red("x"), code: ansiRed},
		"green": {got: s.green("x"), code: ansiGreen},
		"cyan":  {got: s.cyan("x"), code: ansiCyan},
		"dim":   {got: s.dim("x"), code: ansiDim},
	} {
		if !strings.Contains(tc.got, tc.code) || !strings.Contains(tc.got, ansiReset) {
			t.Errorf("%s: expected %q plus reset, got %q", name, tc.code, tc.got)
		}
	}
}

func TestStyleStatusVocabulary(t *testing.T) {
	s := newStyle(true)
	cases := map[string]struct {
		want  string
		plain bool
	}{
		"improved":          {want: "✓ improved", plain: false},
		"published":         {want: "✓ published", plain: false},
		"approved":          {want: "✓ approved", plain: false},
		"new":               {want: "+ new", plain: false},
		"error":             {want: "✗ error", plain: false},
		"validation_failed": {want: "✗ validation_failed", plain: false},
		"needs_human":       {want: "? needs_human", plain: false},
		"always_ask":        {want: "? always_ask", plain: false},
		"unchanged":         {want: "· unchanged", plain: false},
		"skipped":           {want: "· skipped", plain: false},
		"unknown_status":    {want: "unknown_status", plain: true},
	}
	for word, tc := range cases {
		got := s.status(word)
		if !strings.Contains(got, tc.want) {
			t.Errorf("status(%q) = %q, want containing %q", word, got, tc.want)
		}
		if tc.plain && strings.Contains(got, "\033") {
			t.Errorf("status(%q) must be plain, got %q", word, got)
		}
	}
	if got := (style{}).status("improved"); got != "improved" {
		t.Errorf("disabled style must keep the bare word, got %q", got)
	}
}

func TestPlainLenSkipsANSI(t *testing.T) {
	if got := plainLen("\033[32m✓ improved\033[0m"); got != 10 {
		t.Errorf("plainLen = %d, want 10", got)
	}
	if got := plainLen("было:"); got != 5 {
		t.Errorf("plainLen cyrillic = %d, want 5", got)
	}
}

func TestWrapText(t *testing.T) {
	long := strings.Repeat("слово ", 12) // 72 runes incl trailing space
	lines := strings.Split(wrapText(long, 30), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected wrapped lines, got %q", lines)
	}
	for _, ln := range lines {
		if plainLen(ln) > 30 {
			t.Errorf("line exceeds width 30: %q (%d)", ln, plainLen(ln))
		}
	}
	// Existing newlines are honored.
	if got := wrapText("а\nб", 30); got != "а\nб" {
		t.Errorf("newlines must pass through, got %q", got)
	}
	// Non-positive width returns the text unchanged.
	if got := wrapText("а б в", 0); got != "а б в" {
		t.Errorf("width<=0 must not wrap, got %q", got)
	}
}

func testArtifact() *autoclone.DiffArtifact {
	return &autoclone.DiffArtifact{
		Applied: true,
		Fields: []autoclone.FieldDiff{
			{
				Field: "title", Status: "improved",
				Before: json.RawMessage(`"Junior разработчик"`),
				After:  json.RawMessage(`"Middle Python-разработчик (FastAPI, PostgreSQL)"`),
				Reason: "усиление заголовка под ключевые запросы",
			},
			{
				Field: "about", Status: "unchanged",
				Before: json.RawMessage(`"Опытный разработчик"`),
				Reason: "правок не требуется",
			},
		},
	}
}

func TestPrintImproveDiffPlainMatchesTable(t *testing.T) {
	var buf bytes.Buffer
	o := out{stdout: &buf}
	if err := printImproveDiff(o, testArtifact(), false); err != nil {
		t.Fatalf("printImproveDiff: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"FIELD", "STATUS", "БЫЛО", "СТАЛО", "ПРИЧИНА", "improve diff (применено)"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\033[") {
		t.Errorf("plain output must have no ANSI codes, got:\n%s", out)
	}
}

func TestPrintImproveDiffTTYBlocks(t *testing.T) {
	var buf bytes.Buffer
	o := out{stdout: &buf, st: newStyle(true)}
	if err := printImproveDiff(o, testArtifact(), false); err != nil {
		t.Fatalf("printImproveDiff: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"✓ improved", "было:", "стало:", "причина:", "· unchanged", ansiGreen} {
		if !strings.Contains(out, want) {
			t.Errorf("TTY blocks must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FIELD") || strings.Contains(out, "БЫЛО") {
		t.Errorf("TTY blocks must not render the plain table, got:\n%s", out)
	}
}

func TestPrintStyledTableTTY(t *testing.T) {
	var buf bytes.Buffer
	o := out{stdout: &buf}
	st := newStyle(true)
	o.printStyledTable(st, []string{"ACCOUNT", "STATUS"}, [][]string{
		{"main", st.status("published")},
		{"other", st.status("draft")},
	})
	out := buf.String()
	if !strings.Contains(out, ansiDim+"ACCOUNT") || !strings.Contains(out, "✓ published") {
		t.Errorf("styled table: dim header + colored status expected, got:\n%s", out)
	}
	if strings.Contains(out, "\t") {
		t.Errorf("styled table must not contain tabs, got:\n%s", out)
	}
	// Columns align on plain widths: both status values start at the same column.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	row1, row2 := lines[2], lines[3]
	if plainLen(strings.Split(row1, "main")[0]) != plainLen(strings.Split(row2, "other")[0]) {
		t.Errorf("columns misaligned:\n%s\n%s", row1, row2)
	}
}
