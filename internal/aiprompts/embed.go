// Package aiprompts embeds the builtin system prompts: one Markdown file per
// AI task with TOML front-matter (id, version, task). TOML overrides in
// [ai.tasks.<task>] replace these defaults wholesale (inline | file, never both).
package aiprompts

import "embed"

// FS holds vacancy_scoring.md and the seven sibling task prompts.
//
//go:embed *.md
var FS embed.FS
