// Package schemas embeds the strict JSON Schema of every AI task verdict.
// One schema per task serves both sides of the pipeline: it is sent verbatim
// as the structured-output contract to OpenAI-compatible dialects and used
// for mandatory local validation of every verdict regardless of dialect.
//
// The schemas intentionally stay inside the strict-mode keyword subset
// (type, enum, properties, required, additionalProperties, items, minimum,
// maximum, nullable type arrays) so the same bytes work against OpenAI,
// OpenRouter, vLLM, Ollama and other openai-chat-compatible gateways.
package schemas

import "embed"

// FS holds the task schemas (vacancy_scoring.json, ...).
//
//go:embed *.json
var FS embed.FS
