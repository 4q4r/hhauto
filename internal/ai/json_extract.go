package ai

import (
	"encoding/json"
	"strings"
)

// ExtractJSON pulls a JSON value out of an LLM response that may wrap it in
// markdown fences or surround it with prose. Attempts, in order:
//  1. the content is already valid JSON — returned as is
//  2. the first ```json ... ``` fenced block
//  3. the first ``` ... ``` fenced block
//  4. from the first '{' to the last '}'
//  5. from the first '[' to the last ']'
//
// Every candidate must pass json.Valid; ok=false when none does.
func ExtractJSON(content string) (json.RawMessage, bool) {
	if json.Valid([]byte(content)) {
		return json.RawMessage(content), true
	}
	for _, fence := range []string{"```json", "```"} {
		if start := strings.Index(content, fence); start != -1 {
			start += len(fence)
			if end := strings.Index(content[start:], "```"); end != -1 {
				if candidate := strings.TrimSpace(content[start : start+end]); json.Valid([]byte(candidate)) {
					return json.RawMessage(candidate), true
				}
			}
		}
	}
	for _, pair := range [][2]byte{{'{', '}'}, {'[', ']'}} {
		start := strings.IndexByte(content, pair[0])
		end := strings.LastIndexByte(content, pair[1])
		if start != -1 && end > start {
			if candidate := content[start : end+1]; json.Valid([]byte(candidate)) {
				return json.RawMessage(candidate), true
			}
		}
	}
	return nil, false
}
