package cli

// T53 TTY styling: semantic colors for the human-readable Russian console
// output. Red = error, green = success/improved/applied, cyan = info,
// yellow = warn, dim = secondary fields (hash, account) and labels.
// Colors survive only on a terminal without --no-color, NO_COLOR or
// TERM=dumb; a disabled style passes text through unchanged, so pipes,
// journald and docker logs stay byte-identical to the plain output.

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ANSI SGR sequences: plain 3-bit colors plus dim, reset closes the run.
const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiDim    = "\033[2m"
)

// style renders colored CLI fragments; the zero value is disabled and
// passes everything through, so tests and non-TTY writers see plain text.
type style struct {
	enabled bool
}

// newStyle builds a style, honoring the global --no-color flag.
func newStyle(enabled bool) style {
	return style{enabled: enabled && !flags.noColor}
}

// colorEnvDisabled reports the environment's plain-output request
// (NO_COLOR, TERM=dumb).
func colorEnvDisabled() bool {
	return os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
}

// detectStyle resolves colors for one sink: a terminal without
// NO_COLOR/TERM=dumb (and without --no-color, checked in newStyle).
func detectStyle(w io.Writer) style {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) || colorEnvDisabled() {
		return style{}
	}
	return newStyle(true)
}

// colorize wraps text in an SGR pair when colors are enabled.
func (s style) colorize(code, text string) string {
	if !s.enabled {
		return text
	}
	return code + text + ansiReset
}

func (s style) red(text string) string   { return s.colorize(ansiRed, text) }
func (s style) green(text string) string { return s.colorize(ansiGreen, text) }
func (s style) cyan(text string) string  { return s.colorize(ansiCyan, text) }
func (s style) dim(text string) string   { return s.colorize(ansiDim, text) }

// status renders a status word with its semantic color and glyph:
// improved/applied/published/modified/approved green ✓, new cyan +,
// error/validation_failed red ✗, needs_human/always_ask yellow ?,
// unchanged/skipped dim ·. Unknown words pass through plain. Glyphs
// render only with colors, so plain output keeps the bare word (grep-
// and diff-friendly).
func (s style) status(word string) string {
	var glyph, code string
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "improved", "applied", "published", "modified", "approved":
		glyph, code = "✓", ansiGreen
	case "new":
		glyph, code = "+", ansiCyan
	case "error", "validation_failed":
		glyph, code = "✗", ansiRed
	case "needs_human", "always_ask":
		glyph, code = "?", ansiYellow
	case "unchanged", "skipped":
		glyph, code = "·", ansiDim
	default:
		return word
	}
	if !s.enabled {
		return word
	}
	return code + glyph + " " + word + ansiReset
}

// ttyWidth returns the sink's terminal width; 80 when the sink is not a
// terminal or the size is unavailable.
func ttyWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 80
	}
	col, _, err := term.GetSize(int(f.Fd()))
	if err != nil || col <= 0 {
		return 80
	}
	return col
}

// plainLen counts visible runes, skipping ANSI SGR sequences, so table
// padding never counts color codes toward a column width.
func plainLen(s string) int {
	n := 0
	inSeq := false
	for _, r := range s {
		switch {
		case inSeq:
			if r == 'm' {
				inSeq = false
			}
		case r == '\033':
			inSeq = true
		default:
			n++
		}
	}
	return n
}

// padCell pads a (possibly styled) cell to the plain-text width.
func padCell(cell string, width int) string {
	pad := width - plainLen(cell)
	if pad <= 0 {
		return cell
	}
	return cell + strings.Repeat(" ", pad)
}

// wrapText greedy-wraps text to width runes, honoring existing newlines;
// words longer than width pass through intact.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		out = append(out, wrapLine(para, width)...)
	}
	return strings.Join(out, "\n")
}

// wrapLine wraps one physical line; blank lines pass through.
func wrapLine(line string, width int) []string {
	words := strings.Fields(line)
	if len(words) == 0 {
		return []string{line}
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if plainLen(cur)+1+plainLen(w) <= width {
			cur += " " + w
			continue
		}
		lines = append(lines, cur)
		cur = w
	}
	return append(lines, cur)
}
