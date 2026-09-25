package ai

import "strings"

// taskNames maps every valid TaskName to itself; taskByName validates
// configuration strings against the nine known tasks.
var taskNames = map[TaskName]struct{}{
	TaskVacancyScoring: {},
	TaskCoverLetter:    {},
	TaskChatReply:      {},
	TaskTestAnswer:     {},
	TaskWarmupPing:     {},
	TaskClassification: {},
	TaskContactExtract: {},
	TaskLinkDetection:  {},
	TaskResumeImprove:  {},
}

func taskByName(name string) (TaskName, bool) {
	t := TaskName(strings.ToLower(strings.TrimSpace(name)))
	_, ok := taskNames[t]
	return t, ok
}

// SchemaName is the structured-output schema name sent to OpenAI-compatible
// dialects (restricted to [a-zA-Z0-9_-], max 64 chars).
func SchemaName(task TaskName) string { return string(task) }
