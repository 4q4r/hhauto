package hhapi

// Search-snippet extraction (T81). The SSR search item carries the vacancy's
// substance in a snippet node (live shape: snippet.{req,resp,cond,skill,
// desc}); vacancyFromItem fills the typed Vacancy snippet fields from it so
// the scoring prompt can point at real requirements instead of the title.
// Live fact-check 2026-09-30: testdata/live/search_item_badges.json.

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

// snippetCap is the per-field snippet cap in runes — snippets are short,
// the cap only guards pathological payloads (the scoring prompt rides on
// these fields).
const snippetCap = 500

// anyTagRe strips remaining markup (same treatment as the description
// renderer; snippets are single-line, so newlines carry no structure).
var anyTagRe = regexp.MustCompile(`<[^>]*>`)

// snippetText reads one snippet field and normalizes it to capped plain
// text: markup stripped, entities decoded, all whitespace collapsed to
// single spaces.
func snippetText(sn map[string]json.RawMessage, key string) string {
	if sn == nil {
		return ""
	}
	s := str(sn, key)
	if s == "" {
		return ""
	}
	s = html.UnescapeString(anyTagRe.ReplaceAllString(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > snippetCap {
		return string(r[:snippetCap]) + "…"
	}
	return s
}
