package autoclone

// T93 (б): the deterministic role-family post-check. The improve verdict
// for the title must keep the resume's role family — the family the base
// resume's title and key skills name (its current positioning; for a
// clone that is the lineage parent). A title that switches the family
// (the observed drift: «Backend-разработчик» on a DevOps-family clone)
// is structural damage every other gate reads as valid — so this check
// fails it deterministically into needs_human, never auto-applies.
//
// The registry is deliberately conservative: only unambiguous family
// words match, and a title with NO family tokens at all never conflicts
// (false positives cost the owner real verdicts; a missed drift costs
// one parked review).

import (
	"encoding/json"
	"sort"
	"strings"
)

// roleFamilies maps family names to their token lists (lowercase,
// substring matching over the title and key-skills text).
var roleFamilies = []struct {
	name   string
	tokens []string
}{
	{"devops", []string{"devops", "devsecops", "sre", "site reliability", "инфраструктур"}},
	{"backend", []string{"backend", "back-end", "бэкенд"}},
	{"fullstack", []string{"fullstack", "full-stack", "full stack", "фуллстек"}},
	{"frontend", []string{"frontend", "front-end", "фронтенд"}},
	{"qa", []string{"qa", "тестиров"}},
}

// titleFamilies returns the role families the text (a title, optionally
// plus key skills) names. Empty when nothing matches.
func titleFamilies(text string) map[string]bool {
	t := strings.ToLower(text)
	var out map[string]bool
	for _, f := range roleFamilies {
		for _, tok := range f.tokens {
			if strings.Contains(t, tok) {
				if out == nil {
					out = map[string]bool{}
				}
				out[f.name] = true
				break
			}
		}
	}
	return out
}

// familyConflict reports a role-family switch: both sides name families
// and the sets are disjoint.
func familyConflict(base, candidate map[string]bool) bool {
	if len(base) == 0 || len(candidate) == 0 {
		return false
	}
	for f := range candidate {
		if base[f] {
			return false
		}
	}
	return true
}

// familyNames renders a family set sorted (deterministic reasons/logs).
func familyNames(families map[string]bool) []string {
	out := make([]string, 0, len(families))
	for f := range families {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// titleTextOf unwraps the stored title condition format to the plain
// string (the post-check input).
func titleTextOf(raw json.RawMessage) string {
	return conditionString(raw)
}

// familyAnchorText is the base positioning text the family derives from:
// the base resume's title plus its key skills.
func familyAnchorText(title, keySkills string) string {
	return strings.TrimSpace(title + " " + keySkills)
}
