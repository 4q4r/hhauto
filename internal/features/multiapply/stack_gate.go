// T71 cheap stack gate: a deterministic pre-AI check. The 2026-09-30 night
// applied to two Erlang vacancies because the AI scorer was disabled and the
// OR-union query matched their descriptions. This gate skips a vacancy whose
// NAME names languages that are all absent from the candidate's profile text
// — before any expensive work (AI scoring, popups, letters).
//
// The gate is conservative by design:
//   - it reads the vacancy NAME only (names parse reliably; search items
//     carry no description);
//   - it acts only when the profile text itself names at least one known
//     language (positive evidence the text carries the candidate's stack);
//   - an unmarked name always passes;
//   - any single intersection with the profile passes.
// The AI relevance filter behind it stays the fine-grained authority.

package multiapply

import (
	"regexp"
	"strings"
)

// stackLanguage is one marker row: name patterns detecting the language in a
// vacancy name and profile patterns detecting it in the profile text. Latin
// patterns anchor at word boundaries (\b — "java" must not hit
// "JavaScript"); Cyrillic patterns ("1с", "питон") match as substrings —
// Go's ASCII \b never borders a Cyrillic letter.
type stackLanguage struct {
	name    []*regexp.Regexp
	profile []*regexp.Regexp
}

func compileStack(pats ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(pats))
	for _, p := range pats {
		out = append(out, regexp.MustCompile(`(?i)`+p))
	}
	return out
}

// same returns the patterns for both surfaces.
func same(pats ...string) (name, profile []*regexp.Regexp) {
	return compileStack(pats...), compileStack(pats...)
}

var stackLanguages = []stackLanguage{
	mustStack(`\berlang\b`, `эрланг`),
	mustStack(`\belixir\b`),
	mustStack(`\bruby\b`, `\brails\b`),
	mustStack(`\bphp\b`, `битрикс`, `\bbitrix\b`, `laravel`, `symfony`),
	mustStack(`1[сc]`),
	mustStack(`\bjava\b`),
	mustStack(`\bc#`, `csharp`),
	mustStack(`\.net`),
	mustStack(`\bkotlin\b`, `котлин`),
	mustStack(`\bswift\b`),
	mustStack(`\bscala\b`),
	mustStack(`\bperl\b`),
	mustStack(`\bhaskell\b`),
	mustStack(`\bclojure\b`),
	mustStack(`\blua\b`),
	mustStack(`\bgroovy\b`),
	mustStack(`\bobjective-?c\b`),
	mustStack(`\bdelphi\b`, `\bpascal\b`),
	mustStack(`\bc\+\+`, `\bcpp\b`),
	mustStack(`\bgo\b`, `\bgolang\b`),
	mustStack(`\bpython\b`, `питон`),
	mustStack(`\brust\b`),
	mustStack(`\bjavascript\b`, `\btypescript\b`, `\bjs\b`, `\bts\b`),
	mustStack(`\bflutter\b`, `\bdart\b`),
}

func mustStack(pats ...string) stackLanguage {
	name, profile := same(pats...)
	return stackLanguage{name: name, profile: profile}
}

// stackProfileCarriesStack reports whether the profile text names at least
// one known language — the gate's positive-evidence precondition.
func stackProfileCarriesStack(profileText string) bool {
	for _, lang := range stackLanguages {
		for _, re := range lang.profile {
			if re.MatchString(profileText) {
				return true
			}
		}
	}
	return false
}

// stackGateSkip returns the skip reason for a hard name-vs-profile stack
// mismatch, "" when the vacancy passes (unmarked name, an intersection, or
// a stack-less profile text).
func stackGateSkip(name, profileText string) string {
	if strings.TrimSpace(profileText) == "" || !stackProfileCarriesStack(profileText) {
		return ""
	}
	matched := false
	for _, lang := range stackLanguages {
		hit := false
		for _, re := range lang.name {
			if re.MatchString(name) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		matched = true
		for _, re := range lang.profile {
			if re.MatchString(profileText) {
				return "" // this name language is also the candidate's: pass
			}
		}
	}
	if matched {
		return "stack_mismatch"
	}
	return ""
}
