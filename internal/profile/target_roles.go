package profile

// T93 (а): the deterministic target-roles vocabulary for the improve
// tasks. profile.md's «Целевые роли» section is the owner's own list of
// the roles the search should hit; the improve input's target_roles
// parameter never carried it (RC-1), so the model resolved the resume's
// role family from the profile's dominant facts alone.

import "strings"

// TargetRoles extracts the «Целевые роли» section of a profile.md text as
// a flat token list: bullets are split on «/» and «,», a «Метка:» prefix
// is stripped, tokens are trimmed. A missing or empty section returns nil
// (the improve run continues without the parameter, as before).
func TargetRoles(text string) []string {
	section := markdownSection(text, "целевые роли")
	if section == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(section, "\n") {
		t := strings.TrimSpace(line)
		if len(t) == 0 || (t[0] != '-' && t[0] != '*' && !strings.HasPrefix(t, "•")) {
			continue
		}
		t = strings.TrimLeft(t, "-*• ")
		for _, part := range strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == ',' }) {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			if i := strings.LastIndex(part, ":"); i >= 0 {
				part = strings.TrimSpace(part[i+1:])
			}
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// markdownSection returns the body of the first heading whose text
// contains titleSubstr (case-insensitive), up to the next heading of any
// level. Empty when no such heading exists.
func markdownSection(text, titleSubstr string) string {
	lower := strings.ToLower(titleSubstr)
	var b strings.Builder
	in := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			if in {
				break // the next heading closes the section
			}
			head := strings.ToLower(strings.TrimLeft(t, "# "))
			// «НЕ целевые …» is the exclude list — never a target match.
			in = !strings.HasPrefix(head, "не ") && strings.Contains(head, lower)
			continue
		}
		if in {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
