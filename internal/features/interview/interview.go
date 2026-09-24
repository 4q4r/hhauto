// Package interview implements the interview-detection branch of the chat
// workflow (WF3): from an "invitation"-classified chat message it extracts
// meeting links (AI link detection) and a date/time hint (regex), persists
// links into interview_links and emits the interview_found webhook. It never
// answers the recruiter — milestones are human decisions; the caller escalates
// the chat state.
package interview

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features/kit11"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// FeatureState feature name used for per-chat interview state.
const featureName = "interview"

// maxContextSnippet bounds the untrusted text echoed into the AI input.
const maxContextSnippet = 500

// Detector extracts interview facts from a chat message.
type Detector struct {
	DB     *store.DB
	AI     ai.Runner
	Notify webhook.Notifier
	Cfg    config.InterviewDetect
	Log    *slog.Logger
	Now    func() time.Time
}

// Result reports what Detect found and stored.
type Result struct {
	// Links are the interview links stored (or upserted) for this message.
	Links []store.InterviewLink
	// When is the extracted date/time hint, "" when none was found.
	When string
}

// datePatterns are tolerant Russian date/time hints, tried in order; the
// first match wins (most specific first: absolute date, then day word, then
// weekday, then time).
var datePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b\d{1,2}[./]\d{1,2}(?:[./]\d{2,4})?\b`),
	regexp.MustCompile(`(?i)\b(?:завтра|послезавтра|сегодня)\b`),
	regexp.MustCompile(`(?i)\b(?:понедельник|вторник|сред[ау]|четверг|пятниц[уа]|суббот[уа]|воскресенье)\b`),
	regexp.MustCompile(`\b\d{1,2}:\d{2}\b`),
}

// Detect inspects one inbound message of the chat. chatRowID is the store
// chats.id (0 when unknown); chatikID is the platform chat id used in
// feature-state keys. When the interview feature is disabled it is a no-op.
func (d *Detector) Detect(ctx context.Context, accountID string, chatRowID int64, chatikID, messageText string) (Result, error) {
	if !d.Cfg.Enabled {
		return Result{}, nil
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}

	res := Result{}
	urls := schemeURLs(messageText)
	if len(urls) > 0 && d.AI != nil {
		links, err := d.detectLinks(ctx, accountID, chatRowID, messageText, urls, log)
		if err != nil {
			return Result{}, err
		}
		res.Links = links
	}

	res.When = extractWhen(messageText)
	if res.When != "" && chatikID != "" {
		if err := d.DB.FeatureStateSet(ctx, featureName, accountID, "date:"+chatikID, res.When); err != nil {
			return Result{}, fmt.Errorf("interview: persist date: %w", err)
		}
	}

	if len(res.Links) > 0 || res.When != "" {
		if d.Notify != nil {
			payload := map[string]any{"chat": chatikID, "when": res.When}
			if len(res.Links) > 0 {
				ls := make([]string, 0, len(res.Links))
				for _, l := range res.Links {
					ls = append(ls, l.URL)
				}
				payload["links"] = ls
			}
			d.Notify.Emit(webhook.Event{
				Type:      webhook.EvInterviewFound,
				AccountID: accountID,
				Time:      now(),
				Message:   "Обнаружено приглашение на собеседование",
				Payload:   payload,
			})
		}
	}
	return res, nil
}

// detectLinks runs link detection for every URL candidate and stores all
// low/medium-risk links. High-risk links go to the review queue instead.
func (d *Detector) detectLinks(ctx context.Context, accountID string, chatRowID int64, text string, urls []string, log *slog.Logger) ([]store.InterviewLink, error) {
	around := text
	if len(around) > maxContextSnippet {
		around = around[:maxContextSnippet]
	}
	input := map[string]any{
		"urls": urls,
		"context": map[string]any{
			"source": "chat_message",
			"around": around,
		},
	}
	raw, _, err := d.AI.Run(ctx, accountID, "chat", fmt.Sprint(chatRowID), "", ai.TaskLinkDetection, input)
	if err != nil {
		return nil, fmt.Errorf("interview: link detection: %w", err)
	}
	var verdict struct {
		ai.Common
		Links []struct {
			URL  string `json:"url"`
			Kind string `json:"kind"`
			Risk string `json:"risk"`
		} `json:"links"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return nil, fmt.Errorf("interview: link verdict decode: %w", err)
	}

	now := d.Now
	if now == nil {
		now = time.Now
	}
	out := make([]store.InterviewLink, 0, len(verdict.Links))
	for _, l := range verdict.Links {
		if l.URL == "" {
			continue
		}
		if l.Risk == "high" {
			if _, err := d.DB.ReviewItemsAdd(ctx, accountID, "phishing_suspect",
				"interview: link detector flagged high risk",
				mustJSON(map[string]any{"url": l.URL, "risk": l.Risk})); err != nil {
				return nil, fmt.Errorf("interview: review add: %w", err)
			}
			log.Warn("interview: high-risk link skipped", "url", l.URL, "account", accountID)
			continue
		}
		row := store.InterviewLink{
			AccountID:  accountID,
			URL:        l.URL,
			Provider:   l.Kind,
			Status:     "detected",
			DetectedAt: now(),
		}
		if chatRowID > 0 {
			id := chatRowID
			row.ChatID = &id
		}
		if err := d.DB.InterviewLinksUpsert(ctx, &row); err != nil {
			return nil, fmt.Errorf("interview: store link: %w", err)
		}
		out = append(out, row)
	}
	return out, nil
}

// schemeURLs keeps only explicit http(s) links from the text (www. links
// cannot be validated for scheme and are not fetched anywhere anyway).
func schemeURLs(text string) []string {
	var out []string
	for _, u := range kit11.DetectLinks(text) {
		if len(u) > 7 && (u[:7] == "http://" || (len(u) > 8 && u[:8] == "https://")) {
			out = append(out, u)
		}
	}
	return out
}

// extractWhen returns the first date/time hint found in the message.
func extractWhen(text string) string {
	for _, re := range datePatterns {
		if m := re.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

// mustJSON marshals v or returns "{}" on failure (payload is best-effort
// diagnostics, never worth failing a workflow over).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
