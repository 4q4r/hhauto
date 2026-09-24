package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// envVarRe matches ${NAME} placeholders with a sane environment variable name.
var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces every ${NAME} placeholder in s with the value of the
// environment variable NAME. The first unset variable aborts with an error
// that names it; nothing is silently expanded to an empty string.
func expandEnv(s string) (string, error) {
	unset := ""
	out := envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		name := envVarRe.FindStringSubmatch(match)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			if unset == "" {
				unset = name
			}
			return match
		}
		return v
	})
	if unset != "" {
		return "", fmt.Errorf("environment variable %q is not set", unset)
	}
	return out, nil
}

// ExpandPath expands a leading "~" or "~/" to the user's home directory.
// Anything else (including "~user") is returned unchanged.
func ExpandPath(s string) string {
	if s != "~" && !strings.HasPrefix(s, "~/") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return s
	}
	if s == "~" {
		return home
	}
	return filepath.Join(home, s[2:])
}

var (
	tildeBasicRe   = regexp.MustCompile(`(=\s*")~(/[^"\n]*)?"`)
	tildeLiteralRe = regexp.MustCompile(`(=\s*')~(/[^'\n]*)?'`)
)

// expandTildeText expands a leading "~" in TOML string values assigned via
// "=" (both basic "..." and literal '...' strings). Tildes anywhere else in
// the document are left untouched.
func expandTildeText(text string) string {
	text = expandTildeWith(tildeBasicRe, text, '"')
	text = expandTildeWith(tildeLiteralRe, text, '\'')
	return text
}

func expandTildeWith(re *regexp.Regexp, text string, quote byte) string {
	return re.ReplaceAllStringFunc(text, func(match string) string {
		sub := re.FindStringSubmatch(match)
		return sub[1] + ExpandPath("~"+sub[2]) + string(quote)
	})
}
