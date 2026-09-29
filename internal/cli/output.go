// Package cli is the hhauto command line: the integration keystone that
// wires every internal package into a working system (T13). This package
// owns process-level concerns only; business logic lives in the packages
// it wires (config, store, engine, features, hhapi, ai, ...).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// out holds the process output streams. Tests inject buffers.
type out struct {
	stdout io.Writer
	stderr io.Writer
	// st styles TTY output; the zero value (and any non-terminal sink)
	// renders plain text, so tests and pipes see the T49 output as-is.
	st style
}

// printTable renders rows as an aligned table (POSIX tabwriter). header may
// be nil for headerless tables.
func (o out) printTable(header []string, rows [][]string) {
	// Console output is best-effort: write errors (closed pipe) must not
	// turn a successful command into a failure. Flush IS checked: a full
	// tabwriter buffer would otherwise drop rows silently.
	w := tabwriter.NewWriter(o.stdout, 2, 4, 2, ' ', 0)
	if header != nil {
		_, _ = fmt.Fprintln(w, strings.Join(header, "\t"))
		// separator line: dashes under each header column
		seps := make([]string, len(header))
		for i, h := range header {
			seps[i] = strings.Repeat("-", len([]rune(h)))
		}
		_, _ = fmt.Fprintln(w, strings.Join(seps, "\t"))
	}
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush()
}

// printStyledTable renders printTable's layout (header, dashed separator,
// rows) on a terminal: dim header and separator, per-cell colors, columns
// padded on plain-text widths so ANSI codes never skew the alignment.
func (o out) printStyledTable(st style, header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = plainLen(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && plainLen(c) > widths[i] {
				widths[i] = plainLen(c)
			}
		}
	}
	render := func(cells []string) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = padCell(c, widths[i])
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	seps := make([]string, len(header))
	head := make([]string, len(header))
	for i, h := range header {
		seps[i] = st.dim(strings.Repeat("-", plainLen(h)))
		head[i] = st.dim(h)
	}
	o.println(render(head))
	o.println(strings.Join(seps, "  "))
	for _, r := range rows {
		o.println(render(r))
	}
}

// printJSON writes v as indented JSON with a trailing newline.
func (o out) printJSON(v any) error {
	enc := json.NewEncoder(o.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Console writers are best-effort: fmt returns nil error for strings.Reader
// and os.File write failures surface as short output, never as a command
// failure (the exit code must reflect the work, not the terminal).
func (o out) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.stdout, format, args...)
}

func (o out) println(args ...any) {
	_, _ = fmt.Fprintln(o.stdout, args...)
}

func (o out) errf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.stderr, format+"\n", args...)
}

// statusMark renders a doctor check outcome. No ANSI color codes are
// emitted today (--no-color is accepted for forward compatibility), so the
// marks stay plain text.
func statusMark(s string) string {
	switch s {
	case "OK":
		return "OK   "
	case "WARN":
		return "WARN "
	case "FAIL":
		return "FAIL "
	default:
		return "?????"
	}
}
