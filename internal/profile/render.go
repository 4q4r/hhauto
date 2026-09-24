package profile

// Deterministic rendering of a Profile into a trusted system-prompt block.
//
// Section order is canonical (profile, resume, facts) no matter in which
// order the sections are requested, so prompts stay byte-stable for
// provider-side prompt caching. Sections whose content is blank are
// omitted — the prompt never contains empty headers. Every section body is
// truncated to a configurable per-section limit (default DefaultSectionLimit)
// at a line boundary.

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Section selector names for RenderForPrompt.
const (
	SectionProfile = "profile"
	SectionResume  = "resume"
	SectionFacts   = "facts"
)

// DefaultSectionLimit is the per-section body limit in characters (runes)
// used by RenderForPrompt.
const DefaultSectionLimit = 8000

// truncationMarker is appended on its own line when a section body is cut.
const truncationMarker = "…[усечено]"

// RenderForPrompt renders the requested sections of p as deterministic
// prompt text. Without arguments all non-empty sections are rendered.
// Unknown section names are ignored.
func RenderForPrompt(p Profile, sections ...string) string {
	return RenderForPromptLimit(p, DefaultSectionLimit, sections...)
}

// RenderForPromptLimit is RenderForPrompt with an explicit per-section body
// limit in runes; a limit of 0 or less disables truncation.
func RenderForPromptLimit(p Profile, limit int, sections ...string) string {
	want := make(map[string]bool, len(sections))
	for _, s := range sections {
		want[s] = true
	}
	all := len(want) == 0

	var blocks []string
	if all || want[SectionProfile] {
		if body := truncateLines(p.ProfileText, limit); strings.TrimSpace(body) != "" {
			blocks = append(blocks, section("ПРОФИЛЬ КАНДИДАТА", body))
		}
	}
	if all || want[SectionResume] {
		if body := truncateLines(p.ResumeDigest, limit); strings.TrimSpace(body) != "" {
			blocks = append(blocks, section("РЕЗЮМЕ", body))
		}
	}
	if all || want[SectionFacts] {
		if body := factsBody(p.Facts, limit); body != "" {
			blocks = append(blocks, section("ФАКТЫ ПОЛЬЗОВАТЕЛЯ", body))
		}
	}
	return strings.Join(blocks, "\n\n")
}

func section(header, body string) string {
	return "### " + header + "\n" + strings.TrimRight(body, " \t\r\n")
}

// factsBody renders Q&A pairs ("В: q" / "О: a") sorted by question, as one
// block truncated to limit.
func factsBody(facts map[string]string, limit int) string {
	if len(facts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, "В: "+k+"\nО: "+facts[k])
	}
	return truncateLines(strings.Join(pairs, "\n\n"), limit)
}

// truncateLines clips s to at most limit runes at a line boundary: the
// partially kept trailing line is dropped and a visible marker is appended
// on a new line. Text without newlines is hard-cut at the limit. A limit
// of 0 or less disables truncation.
func truncateLines(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	cut := string([]rune(s)[:limit])
	if idx := strings.LastIndexByte(cut, '\n'); idx >= 0 {
		cut = cut[:idx]
	}
	return strings.TrimRight(cut, " \t\r\n") + "\n" + truncationMarker
}
