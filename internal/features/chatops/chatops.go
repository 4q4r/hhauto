// Package chatops implements the chat workflows: WF3 (inbox sync +
// AI classification + routing), WF4 (hh internal tests, one-shot submit with
// RECONCILE), WF7 (auto-leave on rejection, disabled by default) and the
// hand-offs to WF5 (external forms), WF6 (contacts) and WF9 (warmup ladder).
//
// Golden rule: LLMs classify and draft; Go code decides and executes. Every
// outgoing side effect is guarded by an intents write (before the effect),
// links are forbidden in outgoing text, and ambiguous mutations are
// reconciled by reading state — never blindly repeated.
package chatops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/contacts"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/interview"
	"hhauto/internal/features/kit11"
	"hhauto/internal/features/testsolve"
	"hhauto/internal/features/warmup"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

const featureName = "chatops"

// Behavior constants. Values without a config knob are deliberate package
// defaults; the orchestrator may promote them to config later (additive
// config change, no contract break).
const (
	// ReplyMinConfidence is the auto-send confidence threshold for drafts
	// when no dedicated config field exists.
	ReplyMinConfidence = 0.8
	// ClassMinConfidence gates auto-routing on the classification verdict:
	// below this the message goes to review instead of any automated branch
	// (wave-3 review #9).
	ClassMinConfidence = 0.75
	// answerChoiceMinConfidence / answerMultiMinConfidence gate test answers:
	// single-question batches (choice) and multi-question batches (multiple)
	// have different thresholds (wave-3 review #9).
	answerChoiceMinConfidence = 0.75
	answerMultiMinConfidence  = 0.85
	// maxSyncPages bounds one ListChats pagination pass.
	maxSyncPages = 10
	// historyLimit is the per-chat message window synced/considered.
	historyLimit = 300
	// maxReplyHistory bounds the chat context passed to chat_reply.
	maxReplyHistory = 20
	// leave delay window (humanized): rejected chats are left after a
	// randomized 30m..2h wait so the departure does not look mechanical.
	leaveDelayMin = 30 * time.Minute
	leaveDelayMax = 2 * time.Hour
	// maxLeaveTries bounds leave attempts (reconcile-confirmed retries).
	maxLeaveTries = 5
	// sendRetries: bounded same-idempotency-key retries inside one handler
	// run (safe for chatik sends per docs/PLAN.md §6).
	sendRetries = 2
	sendBackoff = 2 * time.Second
	// defaultPollInterval when chatops.poll_interval is unset.
	defaultPollInterval = 5 * time.Minute
)

// SyncPayload is the queue payload for queue.KindChatSync.
type SyncPayload struct {
	AccountID string
}

// Feature drives the chat automation for one deployment.
type Feature struct {
	Cfg    config.Config
	DB     *store.DB
	AI     ai.Runner
	Notify webhook.Notifier
	API    kit11.APIResolver
	Queue  queue.Queue // set by RegisterTasks; used for solve_test/collect hand-offs
	// Pacer spaces outgoing mutations (chat sends, leaves). nil = no-op;
	// the engine wiring provides the account pacer.
	Pacer featurekit.Pacer
	Log   *slog.Logger
	Now   func() time.Time
	Rnd   *rand.Rand
	// LeaveDelay overrides the humanized leave delay (tests).
	LeaveDelay func() time.Duration
	// Sleep overrides retry backoff (tests).
	Sleep func(time.Duration)
	// Review is the review-queue fan-out (dedup + needs_human webhook).
	// nil = storage-only adds (unwired tests).
	Review review.Reviewer
	// Stopped reports the kill switch (channel 1/2/3). nil = never stopped
	// (unwired tests). Checked between chat syncs inside one batch.
	Stopped func(ctx context.Context) bool
}

// New wires the feature (internal detector included).
func New(cfg config.Config, db *store.DB, runner ai.Runner, notify webhook.Notifier,
	api kit11.APIResolver, log *slog.Logger) *Feature {
	if log == nil {
		log = slog.Default()
	}
	return &Feature{
		Cfg:    cfg,
		DB:     db,
		AI:     runner,
		Notify: notify,
		API:    api,
		Log:    log,
		Now:    time.Now,
		Rnd:    rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 11)),
		Sleep:  time.Sleep,
	}
}

// Name implements features.Feature.
func (f *Feature) Name() string { return featureName }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.Cfg.Features.ChatOps.Enabled }

// RegisterTasks implements features.Feature: an interval schedule for the
// inbox poll (specs are times-of-day, so the interval is materialized as
// evenly spaced times) plus one boot task per configured account.
func (f *Feature) RegisterTasks(ctx context.Context, q queue.Queue, reg *scheduler.Registry) error {
	f.Queue = q
	interval := defaultPollInterval
	if d, err := time.ParseDuration(f.Cfg.Features.ChatOps.PollInterval); err == nil && d >= time.Minute {
		interval = d
	}
	reg.Add("chatops.poll", scheduler.Spec{TZ: f.Cfg.General.Timezone, Times: intervalTimes(interval)})
	for accountID := range f.Cfg.Accounts {
		if err := q.Enqueue(ctx, queue.Task{
			AccountID: accountID,
			Kind:      queue.KindChatSync,
			Priority:  queue.PrioChats,
			DedupKey:  "chatops-boot",
			Payload:   SyncPayload{AccountID: accountID},
		}); err != nil {
			return fmt.Errorf("chatops: seed sync: %w", err)
		}
	}
	return nil
}

// intervalTimes materializes a poll interval as "HH:MM" times of day.
func intervalTimes(step time.Duration) []string {
	if step < time.Minute {
		step = time.Minute
	}
	var times []string
	for t := time.Duration(0); t < 24*time.Hour && len(times) < 1440; t += step {
		times = append(times, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC).Add(t).Format("15:04"))
	}
	return times
}

// HandleChatSync processes one queue.KindChatSync task: list → upsert →
// message diff → classify latest inbound → route.
func (f *Feature) HandleChatSync(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(SyncPayload)
	if !ok {
		return fmt.Errorf("chatops: bad payload type %T", task.Payload)
	}
	api, err := f.API(p.AccountID)
	if err != nil {
		return fmt.Errorf("chatops: resolve api: %w", err)
	}
	accountID := p.AccountID
	var negStates map[int64]string // RULING R9: negotiation state per vacancy, fetched at most once per sync pass

	for page := 1; page <= maxSyncPages; page++ {
		chats, err := api.ListChats(ctx, page)
		if err != nil {
			return fmt.Errorf("chatops: list chats (page %d): %w", page, err)
		}
		if len(chats) == 0 {
			break
		}
		if negStates == nil {
			for _, c := range chats {
				if c.VacancyID != 0 {
					negStates, err = f.loadNegotiations(ctx, api)
					if err != nil {
						return err
					}
					break
				}
			}
		}
		for _, c := range chats {
			if f.Stopped != nil && f.Stopped(ctx) {
				// Kill switch: finish the batch gracefully — the chat being
				// processed has settled; the rest are not started.
				f.Log.Info("chatops: kill switch tripped — stopping the sync pass", "account", accountID)
				return nil
			}
			if err := f.syncChat(ctx, api, accountID, c, negStates); err != nil {
				if kit11.IsTransient(err) {
					return err // abort the pass; the scheduler retries
				}
				f.Log.Error("chatops: chat sync failed", "account", accountID, "chat", c.ID, "err", err)
			}
		}
	}
	return nil
}

// loadNegotiations fetches the account's negotiations (bounded pagination)
// and maps vacancyID → state; the first occurrence wins. Returns a non-nil
// map so the per-pass cache is fetched at most once.
func (f *Feature) loadNegotiations(ctx context.Context, api hhapi.HHAPI) (map[int64]string, error) {
	out := map[int64]string{}
	for page := 1; page <= 3; page++ {
		items, err := api.Negotiations(ctx, page)
		if err != nil {
			if page == 1 && (kit11.IsTransient(err) || kit11.IsAmbiguous(err)) {
				return nil, err
			}
			break // negotiations are an enrichment: degrade, never fail the sync
		}
		if len(items) == 0 {
			break
		}
		for _, n := range items {
			if _, ok := out[n.VacancyID]; !ok {
				out[n.VacancyID] = n.State
			}
		}
	}
	return out, nil
}

// syncChat processes one chat end-to-end. negStates is the per-pass
// negotiation state cache (vacancyID → state) used for the structural
// rejection signal (RULING R9).
func (f *Feature) syncChat(ctx context.Context, api hhapi.HHAPI, accountID string, c hhapi.Chat, negStates map[int64]string) error {
	mirror := f.mirrorState(ctx, accountID, c.ID)

	// Pending auto-leave (WF7) runs before anything else: rejected chats are
	// visited every cycle until the leave lands or is cancelled.
	if mirror == stateRejected {
		if err := f.maybeLeave(ctx, api, accountID, c, mirror); err != nil {
			if kit11.IsTransient(err) {
				return err
			}
			f.Log.Error("chatops: leave attempt failed", "chat", c.ID, "err", err)
		}
	}

	row := store.Chat{
		AccountID:     accountID,
		ChatikChatID:  c.ID,
		TopicID:       optStr(c.TopicID),
		VacancyID:     optI64(c.VacancyID),
		EmployerName:  optStr(c.EmployerName),
		LastMessageAt: optTime(c.LastMessageAt),
	}
	if err := f.DB.ChatsUpsert(ctx, &row); err != nil {
		return fmt.Errorf("chatops: upsert chat: %w", err)
	}

	msgs, err := api.ChatHistory(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("chatops: chat history %s: %w", c.ID, err)
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].SentAt.Before(msgs[j].SentAt) })

	stored, err := f.DB.MessagesHistory(ctx, row.ID, historyLimit)
	if err != nil {
		return fmt.Errorf("chatops: stored history: %w", err)
	}
	known := make(map[string]bool, len(stored))
	var lastOut time.Time
	for _, m := range stored {
		known[m.MessageID] = true
		if m.Direction == "out" && m.SentAt.After(lastOut) {
			lastOut = m.SentAt
		}
	}
	var newIn []store.Message
	for _, m := range msgs {
		if known[m.ID] {
			if m.Direction == "out" && m.SentAt.After(lastOut) {
				lastOut = m.SentAt
			}
			continue
		}
		sm := store.Message{
			ChatID:    row.ID,
			MessageID: m.ID,
			Direction: m.Direction,
			Author:    optStr(m.Author),
			Text:      m.Text,
			SentAt:    m.SentAt,
		}
		if err := f.DB.MessagesAdd(ctx, &sm); err != nil {
			if kit11.IsUniqueViolation(err) {
				// concurrent sync inserted it first: treat as known
				known[m.ID] = true
				continue
			}
			return fmt.Errorf("chatops: store message: %w", err)
		}
		known[m.ID] = true
		switch m.Direction {
		case "in":
			newIn = append(newIn, sm)
		case "out":
			if m.SentAt.After(lastOut) {
				lastOut = m.SentAt
			}
		}
	}

	row.LastOutgoingAt = optTime(lastOut)
	if err := f.DB.ChatsUpsert(ctx, &row); err != nil {
		return fmt.Errorf("chatops: refresh chat: %w", err)
	}

	// Contacts (WF6): employer card task once per employer + message
	// signature parsing when enabled. Chat-side errors never abort the sync.
	f.contactsHook(ctx, api, accountID, c, row, newIn)

	inbound := len(newIn) > 0
	if inbound {
		if f.Cfg.Warmup.Enabled {
			if err := warmup.ResetLadder(ctx, f.DB, accountID, c.ID); err != nil {
				return fmt.Errorf("chatops: reset ladder: %w", err)
			}
		}
		if mirror == stateWarmupDue {
			if err := f.DB.ChatsUpdateState(ctx, accountID, c.ID, stateActive, nil); err != nil {
				return fmt.Errorf("chatops: reactivate chat: %w", err)
			}
			mirror = f.setMirror(ctx, accountID, c.ID, stateActive)
		}
	}

	if inbound && mirror != stateEscalated && mirror != stateLeft {
		target := newIn[len(newIn)-1]
		// RULING R9 (live contracts): the chat list itself carries the
		// applicant state (resources.negotiation_topics /
		// lastMessage.workflowTransition, live enum "DISCARD") — zero extra
		// requests. The SSR negotiations map stays as the fallback source,
		// and both spellings route through hhapi.IsStructuralRefusal.
		if hhapi.IsStructuralRefusal(c.ApplicantState) || hhapi.IsStructuralRefusal(negStates[derefI64(row.VacancyID)]) {
			// RULING R9: the structural negotiation state is the PRIMARY
			// rejection signal — no AI call, straight to the rejection
			// branch (auto-leave gates still apply downstream).
			f.handleRejection(ctx, accountID, row, classVerdict{
				Category: "rejection",
				Common:   ai.Common{Confidence: 1.0},
			})
			_ = f.DB.MetricsIncr(ctx, "chatops.rejection_structural", kit11.Day(f.now()))
			mirror = stateRejected
		} else {
			verdict, err := f.classify(ctx, accountID, row, target)
			if err != nil {
				if kit11.IsTransient(err) {
					return err
				}
				f.Log.Error("chatops: classification failed", "chat", c.ID, "err", err)
			} else {
				f.route(ctx, api, accountID, row, target, verdict)
			}
		}
	}

	// Warmup (WF9) silence marking for the warmup consumer.
	if f.Cfg.Warmup.Enabled && !inbound && mirror == stateActive && !lastOut.IsZero() {
		ladder, err := warmup.LadderOf(ctx, f.DB, accountID, c.ID)
		if err != nil {
			return err
		}
		silence := time.Duration(f.Cfg.Warmup.SilenceHours) * time.Hour
		if ladder < f.Cfg.Warmup.MaxPings && silence > 0 && f.now().Sub(lastOut) >= silence {
			if err := f.DB.ChatsUpdateState(ctx, accountID, c.ID, stateWarmupDue, nil); err != nil {
				return fmt.Errorf("chatops: mark warmup_due: %w", err)
			}
			// Last read/write of the local mirror: the store state set by
			// setMirror is what the warmup consumer sees.
			_ = f.setMirror(ctx, accountID, c.ID, stateWarmupDue)
		}
	}
	return nil
}

// --- classification + routing ---

// classVerdict mirrors the message_classification schema.
type classVerdict struct {
	ai.Common
	Category string `json:"category"`
}

func (f *Feature) classify(ctx context.Context, accountID string, chat store.Chat, msg store.Message) (classVerdict, error) {
	input := map[string]any{
		"message":  map[string]any{"author": deref(msg.Author), "text": msg.Text},
		"employer": map[string]any{"name": deref(chat.EmployerName)},
	}
	raw, _, err := f.AI.Run(ctx, accountID, "message", msg.MessageID, "", ai.TaskClassification, input)
	if err != nil {
		return classVerdict{}, fmt.Errorf("chatops: classify: %w", err)
	}
	var v classVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return classVerdict{}, fmt.Errorf("chatops: classification decode: %w", err)
	}
	return v, nil
}

func (f *Feature) route(ctx context.Context, api hhapi.HHAPI, accountID string, chat store.Chat, msg store.Message, v classVerdict) {
	needsHuman := v.NeedsHuman || v.InjectionSuspected
	if needsHuman {
		reason := deref(v.NeedsHumanReason)
		if reason == "" && v.InjectionSuspected {
			reason = "injection_suspected"
		}
		f.review(ctx, accountID, "ai_needs_human", reason, map[string]any{
			"chat": chat.ChatikChatID, "message_id": msg.MessageID, "category": v.Category,
		})
		if f.Notify != nil {
			f.Notify.Emit(webhook.Event{
				Type: webhook.EvNeedsHuman, AccountID: accountID, Time: f.now(),
				Message: "Входящее сообщение требует участия человека",
				Payload: map[string]any{"chat": chat.ChatikChatID, "category": v.Category},
			})
		}
	}

	// Wave-3 review #9: low classification confidence → review instead of
	// auto-routing. Human-milestone branches (invitation/offer) still
	// escalate — escalation STOPS automation, it does not add any.
	lowConf := v.Confidence < ClassMinConfidence
	if lowConf && (v.Category == "question" || v.Category == "test_assignment" || v.Category == "rejection") {
		f.review(ctx, accountID, "ai_low_confidence", "classification confidence below auto-routing threshold",
			map[string]any{"chat": chat.ChatikChatID, "message_id": msg.MessageID,
				"category": v.Category, "confidence": v.Confidence})
		_ = f.DB.MetricsIncr(ctx, "chatops.low_confidence", kit11.Day(f.now()))
		return
	}

	switch v.Category {
	case "question":
		if !needsHuman {
			f.handleQuestion(ctx, api, accountID, chat, msg)
		}
	case "test_assignment":
		if !needsHuman {
			f.handleTestAssignment(ctx, api, accountID, chat, msg)
		}
	case "rejection":
		if !needsHuman {
			f.handleRejection(ctx, accountID, chat, v)
		}
	case "invitation":
		f.handleInvitation(ctx, accountID, chat, msg)
	case "offer":
		f.handleOffer(ctx, accountID, chat, msg)
	default:
		// spam / other: no automation
		_ = f.DB.MetricsIncr(ctx, "chatops."+v.Category, kit11.Day(f.now()))
	}
}

// --- question → chat_reply (WF3 auto-reply) ---

func (f *Feature) handleQuestion(ctx context.Context, api hhapi.HHAPI, accountID string, chat store.Chat, msg store.Message) {
	profileText, err := kit11.RenderProfile(ctx, f.Cfg, accountID, f.DB, api)
	if err != nil {
		f.Log.Error("chatops: profile context", "err", err)
		return
	}
	history, err := f.DB.MessagesHistory(ctx, chat.ID, maxReplyHistory)
	if err != nil {
		f.Log.Error("chatops: history for reply", "err", err)
		return
	}
	chatCtx := make([]map[string]any, 0, len(history))
	for _, m := range history {
		chatCtx = append(chatCtx, map[string]any{"author": deref(m.Author), "direction": m.Direction, "text": m.Text})
	}
	input := map[string]any{
		"chat":    chatCtx,
		"message": map[string]any{"author": deref(msg.Author), "text": msg.Text},
		"vacancy": f.vacancyInfo(ctx, api, accountID, chat),
		"profile": profileText,
	}
	raw, _, err := f.AI.Run(ctx, accountID, "message", msg.MessageID, "", ai.TaskChatReply, input)
	if err != nil {
		f.Log.Error("chatops: chat_reply failed", "err", err)
		return
	}
	var verdict struct {
		ai.Common
		Reply  *string `json:"reply"`
		Intent string  `json:"intent"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		f.Log.Error("chatops: chat_reply decode", "err", err)
		return
	}

	draft := deref(verdict.Reply)
	autoReply := f.Cfg.Features.ChatOps.AutoReply == nil || *f.Cfg.Features.ChatOps.AutoReply
	if !autoReply {
		// Drafts are still produced for the review queue; a human sends them.
		f.review(ctx, accountID, "reply_draft", "auto_reply disabled — draft for manual send",
			map[string]any{"chat": chat.ChatikChatID, "draft": draft})
		return
	}
	switch {
	case verdict.InjectionSuspected || verdict.NeedsHuman || draft == "":
		f.review(ctx, accountID, "ai_low_confidence", deref(verdict.NeedsHumanReason),
			map[string]any{"chat": chat.ChatikChatID, "draft": draft, "confidence": verdict.Confidence})
		if f.Notify != nil {
			f.Notify.Emit(webhook.Event{
				Type: webhook.EvNeedsHuman, AccountID: accountID, Time: f.now(),
				Message: "Черновик ответа требует проверки",
				Payload: map[string]any{"chat": chat.ChatikChatID},
			})
		}
		return
	case verdict.Confidence < ReplyMinConfidence:
		f.review(ctx, accountID, "ai_low_confidence", "reply confidence below auto-send threshold",
			map[string]any{"chat": chat.ChatikChatID, "draft": draft, "confidence": verdict.Confidence})
		if f.Notify != nil {
			f.Notify.Emit(webhook.Event{
				Type: webhook.EvNeedsHuman, AccountID: accountID, Time: f.now(),
				Message: "Низкая уверенность ответа — ручная проверка",
				Payload: map[string]any{"chat": chat.ChatikChatID, "confidence": verdict.Confidence},
			})
		}
		return
	case kit11.HasLink(draft):
		// Final NoLinks gate: the platform shadow-bans messages with links.
		f.review(ctx, accountID, "reply_links_forbidden", "draft contains a link — forbidden by platform rules",
			map[string]any{"chat": chat.ChatikChatID, "draft": draft})
		return
	}
	outcome, err := f.sendWithIntent(ctx, api, accountID, chat.ChatikChatID,
		messageIntentKey(accountID, chat.ChatikChatID, msg.MessageID), draft)
	if err != nil {
		f.Log.Error("chatops: reply send", "err", err)
	}
	_ = outcome
	if outcome == sendSent {
		_ = f.DB.MetricsIncr(ctx, "chatops.reply_sent", kit11.Day(f.now()))
	}
}

// --- test_assignment → WF4 (internal) / WF5 hand-off (external) ---

var questionNumRe = regexp.MustCompile(`(?m)^\s*(?:Вопрос\s*)?(\d{1,2})\s*[.)-]\s*(\S.+)$`)

func parseQuestions(text string) []string {
	var out []string
	for _, m := range questionNumRe.FindAllStringSubmatch(text, -1) {
		q := strings.TrimSpace(m[2])
		if q != "" {
			out = append(out, q)
		}
	}
	if len(out) > 0 {
		return out
	}
	// Fallback: lines that look like bare questions.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "?") && len([]rune(line)) > 5 {
			out = append(out, line)
		}
	}
	return out
}

func (f *Feature) handleTestAssignment(ctx context.Context, api hhapi.HHAPI, accountID string, chat store.Chat, msg store.Message) {
	links := testsolve.DetectExternalLinks(msg.Text)
	if len(links) > 0 {
		// WF5 hand-off: external forms are solved by the testsolve package.
		if f.Queue == nil {
			f.Log.Warn("chatops: queue not registered, cannot enqueue solve_test")
			return
		}
		for _, u := range links {
			testID := testsolve.TestIDForURL(u)
			if err := f.Queue.Enqueue(ctx, queue.Task{
				AccountID: accountID,
				Kind:      queue.KindSolveTest,
				Priority:  queue.PrioChats,
				DedupKey:  "solve:" + accountID + ":" + testID,
				Payload: testsolve.SolvePayload{
					AccountID: accountID,
					TestID:    testID,
					URL:       u,
					Source:    "chat_message",
					ChatikID:  chat.ChatikChatID,
					VacancyID: derefI64(chat.VacancyID),
				},
			}); err != nil {
				f.Log.Error("chatops: enqueue solve_test", "err", err)
			}
		}
		return
	}

	questions := parseQuestions(msg.Text)
	if len(questions) == 0 {
		f.review(ctx, accountID, "test_parse_failed", "test_assignment without a parseable question list",
			map[string]any{"chat": chat.ChatikChatID, "message_id": msg.MessageID})
		return
	}
	profileText, err := kit11.RenderProfile(ctx, f.Cfg, accountID, f.DB, api)
	if err != nil {
		f.Log.Error("chatops: profile context for test", "err", err)
		return
	}
	answers, unsure, err := f.answerQuestions(ctx, accountID, questions, profileText)
	if err != nil {
		if kit11.IsTransient(err) {
			f.Log.Warn("chatops: test answers transient failure", "err", err)
			return // next sync re-classifies nothing, but the review below covers silence
		}
		f.Log.Error("chatops: test answers failed", "err", err)
		return
	}
	testID := "hh:" + chat.ChatikChatID
	artifact := mustJSON(map[string]any{
		"test_id":      testID,
		"chat":         chat.ChatikChatID,
		"message_id":   msg.MessageID,
		"vacancy_id":   derefI64(chat.VacancyID),
		"answers":      answers,
		"submitted_at": f.now().UTC().Format(time.RFC3339),
	})

	if unsure {
		if _, aerr := f.DB.ReviewItemsAdd(ctx, accountID, "test_unsure",
			"AI unsure about test answers — test NOT submitted",
			artifactJSONWithAnswers(answers, msg.MessageID)); aerr != nil {
			f.Log.Error("chatops: review add", "err", aerr)
		}
		if err := f.DB.AssessmentsAdd(ctx, &store.Assessment{
			AccountID: accountID,
			VacancyID: chat.VacancyID,
			Kind:      "hh_internal",
			Status:    "escalated",
			Answers:   strPtr(artifactJSONWithAnswers(answers, msg.MessageID)),
		}); err != nil {
			f.Log.Error("chatops: store escalated test", "err", err)
		}
		if f.Notify != nil {
			f.Notify.Emit(webhook.Event{
				Type: webhook.EvNeedsHuman, AccountID: accountID, Time: f.now(),
				Message: "Тест: неуверенные ответы — отправка вручную",
				Payload: map[string]any{"chat": chat.ChatikChatID},
			})
		}
		return
	}

	submitText := formatAnswers(questions, answers)
	if kit11.HasLink(submitText) {
		f.review(ctx, accountID, "reply_links_forbidden", "test answers contain a link — submission blocked",
			map[string]any{"chat": chat.ChatikChatID})
		return
	}

	// One-shot submit: the intent is the hard barrier (WF4 RECONCILE rule).
	intentKey := "test:" + accountID + ":" + testID
	outcome, err := f.sendWithIntent(ctx, api, accountID, chat.ChatikChatID, intentKey, submitText)
	if err != nil {
		f.Log.Error("chatops: test submit", "err", err)
	}
	switch outcome {
	case sendSent, sendDryRun, sendAlreadySent:
		if outcome == sendSent {
			_ = f.DB.MetricsIncr(ctx, "chatops.test_submitted", kit11.Day(f.now()))
			if f.Notify != nil {
				f.Notify.Emit(webhook.Event{
					Type: webhook.EvTestSolved, AccountID: accountID, Time: f.now(),
					Message: "Тест пройден и отправлен",
					Payload: map[string]any{"chat": chat.ChatikChatID, "test_id": testID},
				})
			}
		}
		// Dry-run records no solved/submitted artifact: the assessment row
		// must reflect a fact, and in dry-run nothing was submitted.
		if !f.Cfg.General.DryRun {
			if err := f.recordTest(ctx, accountID, chat, artifact, testID, "solved", "submitted"); err != nil {
				f.Log.Error("chatops: record test", "err", err)
			}
		}
	case sendAmbiguous:
		// RECONCILE by reading the chat: did our answers land?
		sent, rerr := textDelivered(ctx, api, chat.ChatikChatID, submitText)
		if rerr == nil && sent {
			if err := f.recordTest(ctx, accountID, chat, artifact, testID, "solved", "submitted_ambiguous_reconciled"); err != nil {
				f.Log.Error("chatops: record test", "err", err)
			}
			return
		}
		f.review(ctx, accountID, "test_submit_ambiguous",
			"test submit outcome unknown — check the chat before any manual repeat",
			map[string]any{"chat": chat.ChatikChatID, "test_id": testID})
	case sendInFlight, sendFailed:
		// in-flight: another attempt owns it; failed: reviewed by the helper
	}
}

// recordTest persists the assessment row unless an identical artifact is
// already recorded (reconciled re-runs must not duplicate rows).
func (f *Feature) recordTest(ctx context.Context, accountID string, chat store.Chat, artifact, testID, status, result string) error {
	rows, err := f.DB.AssessmentsListByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Kind == "hh_internal" && r.Answers != nil && strings.Contains(*r.Answers, testID) {
			return nil
		}
	}
	return f.DB.AssessmentsAdd(ctx, &store.Assessment{
		AccountID: accountID,
		VacancyID: chat.VacancyID,
		Kind:      "hh_internal",
		Status:    status,
		Answers:   strPtr(artifact),
		Result:    strPtr(result),
	})
}

type aiAnswerEntry struct {
	Question       string `json:"question"`
	Answer         string `json:"answer"`
	BasedOnProfile bool   `json:"based_on_profile"`
}

// answerQuestions runs TaskTestAnswer in batches of ≤5.
func (f *Feature) answerQuestions(ctx context.Context, accountID string, questions []string, profileText string) ([]aiAnswerEntry, bool, error) {
	var out []aiAnswerEntry
	unsure := false
	for start := 0; start < len(questions); start += 5 {
		end := min(start+5, len(questions))
		qs := make([]map[string]any, 0, end-start)
		for i, q := range questions[start:end] {
			qs = append(qs, map[string]any{"id": start + i + 1, "text": q})
		}
		input := map[string]any{"questions": qs, "profile": profileText}
		raw, _, err := f.AI.Run(ctx, accountID, "test", fmt.Sprint(start), "", ai.TaskTestAnswer, input)
		if err != nil {
			return nil, false, fmt.Errorf("chatops: test_answer: %w", err)
		}
		var verdict struct {
			ai.Common
			Answers []aiAnswerEntry `json:"answers"`
		}
		if err := json.Unmarshal(raw, &verdict); err != nil {
			return nil, false, fmt.Errorf("chatops: test verdict decode: %w", err)
		}
		out = append(out, verdict.Answers...)
		if verdict.NeedsHuman || verdict.InjectionSuspected {
			unsure = true
		}
		// Wave-3 review #9: batch confidence gates — choice (one question)
		// ≥0.75, multiple (several questions) ≥0.85.
		threshold := answerChoiceMinConfidence
		if len(qs) > 1 {
			threshold = answerMultiMinConfidence
		}
		if verdict.Confidence < threshold {
			unsure = true
		}
		for _, a := range verdict.Answers {
			if strings.TrimSpace(a.Answer) == "" {
				unsure = true
			}
		}
	}
	return out, unsure, nil
}

// formatAnswers renders answers as a numbered chat message.
func formatAnswers(questions []string, answers []aiAnswerEntry) string {
	byQ := make(map[string]string, len(answers))
	for _, a := range answers {
		byQ[a.Question] = a.Answer
	}
	var b strings.Builder
	for i, q := range questions {
		ans, ok := byQ[q]
		if !ok {
			ans = "?"
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, ans)
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- rejection → WF7 (auto-leave, off by default) ---

func (f *Feature) handleRejection(ctx context.Context, accountID string, chat store.Chat, v classVerdict) {
	conf := v.Confidence
	if err := f.DB.ChatsUpdateState(ctx, accountID, chat.ChatikChatID, stateRejected, &conf); err != nil {
		f.Log.Error("chatops: mark rejected", "err", err)
		return
	}
	f.setMirror(ctx, accountID, chat.ChatikChatID, stateRejected)
	_ = f.DB.MetricsIncr(ctx, "chatops.rejection_detected", kit11.Day(f.now()))

	if !f.Cfg.Features.ChatOps.AutoLeaveRejected {
		return
	}
	if conf < f.Cfg.Features.ChatOps.RejectionMinConf {
		f.Log.Info("chatops: rejection below leave threshold", "chat", chat.ChatikChatID, "conf", conf)
		return
	}
	if f.leaveGated(ctx, accountID, chat) {
		return
	}
	// Humanized delay: leave is executed by a later sync pass.
	delay := f.leaveDelay()
	due := f.now().Add(delay)
	if err := f.DB.FeatureStateSet(ctx, featureName, accountID, "leave:"+chat.ChatikChatID, due.UTC().Format(time.RFC3339)); err != nil {
		f.Log.Error("chatops: schedule leave", "err", err)
	}
	f.Log.Info("chatops: leave scheduled", "chat", chat.ChatikChatID, "due", due)
}

// leaveGated reports do-not-leave / has_offer conditions.
func (f *Feature) leaveGated(ctx context.Context, accountID string, chat store.Chat) bool {
	employer := strings.ToLower(strings.TrimSpace(deref(chat.EmployerName)))
	for _, d := range f.Cfg.Features.ChatOps.DoNotLeave {
		if strings.EqualFold(strings.TrimSpace(d), employer) && employer != "" {
			return true
		}
	}
	// has_offer gate: an escalated chat means interview/offer milestones were
	// seen — never auto-leave those.
	mirror := f.mirrorState(ctx, accountID, chat.ChatikChatID)
	return mirror == stateEscalated
}

// maybeLeave executes a scheduled leave once it is due. It runs while the
// chat still appears in ListChats, which doubles as the RECONCILE signal for
// an ambiguous prior attempt (still listed = still a participant).
func (f *Feature) maybeLeave(ctx context.Context, api hhapi.HHAPI, accountID string, c hhapi.Chat, _ string) error {
	val, ok, err := f.DB.FeatureStateGet(ctx, featureName, accountID, "leave:"+c.ID)
	if err != nil || !ok || val == "" || val == "done" || val == "off" || val == "failed" {
		return err
	}
	due, perr := time.Parse(time.RFC3339, val)
	if perr != nil {
		return nil
	}
	if f.now().Before(due) {
		return nil
	}

	row := store.Chat{AccountID: accountID, ChatikChatID: c.ID, EmployerName: optStr(c.EmployerName)}
	if f.leaveGated(ctx, accountID, row) {
		_ = f.DB.FeatureStateSet(ctx, featureName, accountID, "leave:"+c.ID, "off")
		return nil
	}
	tries, err := f.leaveTries(ctx, accountID, c.ID)
	if err != nil {
		return err
	}
	if tries >= maxLeaveTries {
		_ = f.DB.FeatureStateSet(ctx, featureName, accountID, "leave:"+c.ID, "failed")
		f.review(ctx, accountID, "leave_failed", "auto-leave did not complete after retries",
			map[string]any{"chat": c.ID})
		return nil
	}

	intentID, err := f.DB.IntentsBegin(ctx, "leave:"+c.ID, "pending", val)
	switch {
	case errors.Is(err, store.ErrIntentDone):
		// left earlier; repair the state and stop
		_ = f.DB.ChatsUpdateState(ctx, accountID, c.ID, stateLeft, nil)
		_ = f.DB.FeatureStateSet(ctx, featureName, accountID, "leave:"+c.ID, "done")
		f.setMirror(ctx, accountID, c.ID, stateLeft)
		return nil
	case errors.Is(err, store.ErrIntentPending):
		return nil
	case err != nil:
		return fmt.Errorf("chatops: leave intent: %w", err)
	}

	if f.Cfg.General.DryRun {
		// Dry-run records no "left" fact and leaves the intent re-armable
		// (§6): done would both swallow the real leave forever and suppress
		// maybeLeave via the "done" feature state.
		_ = f.DB.IntentsMarkAbandoned(ctx, intentID)
		f.Log.Info("chatops: dry-run, leave skipped", "chat", c.ID)
		return nil
	}

	// Optional farewell (best-effort, its own message intent).
	if tpl := strings.TrimSpace(f.Cfg.Features.ChatOps.LeaveFarewell); tpl != "" {
		text := strings.ReplaceAll(tpl, "{employer}", c.EmployerName)
		outcome, ferr := f.sendWithIntent(ctx, api, accountID, c.ID,
			messageIntentKey(accountID, c.ID, "leave-farewell"), text)
		// An ambiguous or failed farewell is not fatal: still leave, the
		// farewell is cosmetic.
		_ = outcome
		if ferr != nil {
			f.Log.Warn("chatops: farewell send failed", "chat", c.ID, "err", ferr)
		}
	}

	// Pacer before the leave mutation (wave-3 review #4).
	if err := f.pace(ctx, accountID); err != nil {
		_ = f.DB.IntentsMarkFailed(ctx, intentID)
		return fmt.Errorf("chatops: pacer: %w", err)
	}
	leaveErr := api.LeaveChat(ctx, c.ID)
	switch {
	case leaveErr == nil || errors.Is(leaveErr, hhapi.ErrNotFound):
		if leaveErr != nil {
			// not found → we are already out; treat as success
			_ = f.DB.IntentsMarkFailed(ctx, intentID)
		} else if err := f.DB.IntentsMarkDone(ctx, intentID); err != nil {
			return err
		}
		if err := f.DB.ChatsUpdateState(ctx, accountID, c.ID, stateLeft, nil); err != nil {
			return err
		}
		_ = f.DB.FeatureStateSet(ctx, featureName, accountID, "leave:"+c.ID, "done")
		_ = f.DB.FeatureStateSet(ctx, featureName, accountID, "leavetry:"+c.ID, "0")
		f.setMirror(ctx, accountID, c.ID, stateLeft)
		_ = f.DB.MetricsIncr(ctx, "chatops.leave_executed", kit11.Day(f.now()))
		f.Log.Info("chatops: chat left", "chat", c.ID)
		return nil
	case kit11.IsAmbiguous(leaveErr):
		// Outcome unknown: the failed intent allows a reconcile-confirmed
		// retry on a later cycle; a duplicated LeaveChat is harmless (the
		// second attempt returns "already a participant? no" → ErrNotFound).
		_ = f.DB.IntentsMarkFailed(ctx, intentID)
		_ = f.bumpLeaveTries(ctx, accountID, c.ID)
		return nil
	case kit11.IsTransient(leaveErr):
		_ = f.DB.IntentsMarkFailed(ctx, intentID)
		_ = f.bumpLeaveTries(ctx, accountID, c.ID)
		return leaveErr
	default:
		_ = f.DB.IntentsMarkFailed(ctx, intentID)
		_ = f.bumpLeaveTries(ctx, accountID, c.ID)
		f.Log.Warn("chatops: leave rejected", "chat", c.ID, "err", leaveErr)
		return nil
	}
}

// --- invitation / offer ---

func (f *Feature) handleInvitation(ctx context.Context, accountID string, chat store.Chat, msg store.Message) {
	if det := f.interviewDetector(); det != nil {
		res, err := det.Detect(ctx, accountID, chat.ID, chat.ChatikChatID, msg.Text)
		if err != nil {
			if kit11.IsTransient(err) {
				f.Log.Warn("chatops: interview detect transient", "err", err)
				// escalate anyway: the milestone must stop automatisms
			} else {
				f.Log.Error("chatops: interview detect failed", "err", err)
			}
		} else {
			f.Log.Info("chatops: interview detected", "chat", chat.ChatikChatID, "links", len(res.Links), "when", res.When)
		}
	}
	if err := f.DB.ChatsUpdateState(ctx, accountID, chat.ChatikChatID, stateEscalated, nil); err != nil {
		f.Log.Error("chatops: escalate chat", "err", err)
		return
	}
	f.setMirror(ctx, accountID, chat.ChatikChatID, stateEscalated)
	_ = f.DB.MetricsIncr(ctx, "chatops.interview_found", kit11.Day(f.now()))
}

// interviewDetector builds the interview detector against the CURRENT config
// (the feature struct may be reconfigured in tests after New).
func (f *Feature) interviewDetector() *interview.Detector {
	if !f.Cfg.Features.Interview.Enabled || f.DB == nil || f.AI == nil {
		return nil
	}
	return &interview.Detector{
		DB:     f.DB,
		AI:     f.AI,
		Notify: f.Notify,
		Cfg:    f.Cfg.Features.Interview,
		Log:    f.Log,
		Now:    f.Now,
	}
}

func (f *Feature) handleOffer(ctx context.Context, accountID string, chat store.Chat, msg store.Message) {
	if err := f.DB.ChatsUpdateState(ctx, accountID, chat.ChatikChatID, stateEscalated, nil); err != nil {
		f.Log.Error("chatops: escalate chat", "err", err)
		return
	}
	f.setMirror(ctx, accountID, chat.ChatikChatID, stateEscalated)
	f.review(ctx, accountID, "offer_received", "оффер — решение за человеком",
		map[string]any{"chat": chat.ChatikChatID, "message_id": msg.MessageID, "text": msg.Text})
	if f.Notify != nil {
		f.Notify.Emit(webhook.Event{
			Type: webhook.EvOfferReceived, AccountID: accountID, Time: f.now(),
			Message: "Получен оффер",
			Payload: map[string]any{"chat": chat.ChatikChatID},
		})
	}
	_ = f.DB.MetricsIncr(ctx, "chatops.offer_received", kit11.Day(f.now()))
}

// --- contacts hand-off (WF6) ---

func (f *Feature) contactsHook(ctx context.Context, api hhapi.HHAPI, accountID string, c hhapi.Chat, row store.Chat, newIn []store.Message) {
	if !f.Cfg.Features.Contacts.Enabled {
		return
	}
	vacID := derefI64(row.VacancyID)
	var employerID int64
	if vacID != 0 {
		if info := f.resolveVacancy(ctx, api, accountID, vacID); info != nil {
			employerID = info.EmployerID
		}
	}
	if employerID == 0 {
		return
	}

	if f.Cfg.Features.Contacts.FromCard && f.Queue != nil {
		marker := kit11.CollectQueuedMarker(employerID)
		if queued, _, err := f.DB.FeatureStateGet(ctx, featureName, accountID, marker); err == nil && queued == "" {
			if err := f.Queue.Enqueue(ctx, queue.Task{
				AccountID: accountID,
				Kind:      queue.KindCollectContacts,
				Priority:  queue.PrioApply,
				DedupKey:  fmt.Sprintf("collect:%s:%d", accountID, employerID),
				Payload: contacts.CollectPayload{
					AccountID:    accountID,
					EmployerID:   employerID,
					VacancyID:    vacID,
					VacancyTitle: f.vacancyTitle(ctx, api, accountID, vacID),
				},
			}); err != nil {
				f.Log.Error("chatops: enqueue collect_contacts", "err", err)
			} else {
				_ = f.DB.FeatureStateSet(ctx, featureName, accountID, marker, "1")
			}
		}
	}

	if f.Cfg.Features.Contacts.FromMessages && len(newIn) > 0 {
		text := newIn[len(newIn)-1].Text
		if _, err := contacts.CollectFromMessage(ctx, f.DB, f.AI, accountID, employerID,
			deref(row.EmployerName), text); err != nil {
			f.Log.Warn("chatops: contact extraction failed", "chat", c.ID, "err", err)
		}
	}
}

// --- vacancy cache ---

type vacancyInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	EmployerID int64  `json:"employer_id"`
}

func (f *Feature) resolveVacancy(ctx context.Context, api hhapi.HHAPI, accountID string, vacID int64) *vacancyInfo {
	key := fmt.Sprintf("vac:%d", vacID)
	val, ok, err := f.DB.FeatureStateGet(ctx, featureName, accountID, key)
	if err == nil && ok && val != "" {
		var v vacancyInfo
		if json.Unmarshal([]byte(val), &v) == nil {
			return &v
		}
	}
	if api == nil {
		return nil
	}
	card, err := api.VacancyCard(ctx, vacID)
	if err != nil || card == nil {
		f.Log.Warn("chatops: vacancy card unavailable", "vacancy", vacID, "err", err)
		return nil
	}
	info := vacancyInfo{ID: card.ID, Name: card.Name, EmployerID: card.EmployerID}
	if b, err := json.Marshal(info); err == nil {
		_ = f.DB.FeatureStateSet(ctx, featureName, accountID, key, string(b))
	}
	return &info
}

func (f *Feature) vacancyInfo(ctx context.Context, api hhapi.HHAPI, accountID string, chat store.Chat) map[string]any {
	vacID := derefI64(chat.VacancyID)
	if vacID == 0 {
		return nil
	}
	if info := f.resolveVacancy(ctx, api, accountID, vacID); info != nil {
		return map[string]any{"id": info.ID, "name": info.Name}
	}
	return nil
}

func (f *Feature) vacancyTitle(ctx context.Context, api hhapi.HHAPI, accountID string, vacID int64) string {
	if info := f.resolveVacancy(ctx, api, accountID, vacID); info != nil {
		return info.Name
	}
	return ""
}

// --- send helper: intents + bounded same-key retry + reconcile ---

type sendOutcome int

const (
	sendSent sendOutcome = iota
	sendAlreadySent
	sendInFlight
	sendAmbiguous
	sendFailed
	sendDryRun
)

func messageIntentKey(accountID, chatikID, sourceMessageID string) string {
	return fmt.Sprintf("msg:%s:%s:%s", accountID, chatikID, sourceMessageID)
}

// sendWithIntent performs one guarded chat send: intent before effect,
// bounded same-key retries (chatik idempotency makes them safe), pending
// intents block repeats on ambiguous outcomes, permanent failures are
// reviewed once and marked.
func (f *Feature) sendWithIntent(ctx context.Context, api hhapi.HHAPI, accountID, chatikID, intentKey, text string) (sendOutcome, error) {
	intentID, err := f.DB.IntentsBegin(ctx, intentKey, "pending", text)
	switch {
	case errors.Is(err, store.ErrIntentDone):
		return sendAlreadySent, nil
	case errors.Is(err, store.ErrIntentPending):
		return sendInFlight, nil
	case err != nil:
		return sendFailed, fmt.Errorf("chatops: send intent: %w", err)
	}

	if f.Cfg.General.DryRun {
		// Dry-run leaves the intent re-armable (§6): done means "really
		// sent", so marking it done here would swallow the real send of
		// this message forever (same ruling as warmup's ping intents).
		_ = f.DB.IntentsMarkAbandoned(ctx, intentID)
		f.Log.Info("chatops: dry-run, send skipped", "chat", chatikID, "text", text)
		return sendDryRun, nil
	}

	var sendErr error
	for attempt := 0; attempt <= sendRetries; attempt++ {
		// Pacer before EVERY mutation attempt (wave-3 review #4).
		if err := f.pace(ctx, accountID); err != nil {
			_ = f.DB.IntentsMarkFailed(ctx, intentID)
			return sendFailed, fmt.Errorf("chatops: pacer: %w", err)
		}
		if attempt > 0 {
			f.Sleep(sendBackoff * time.Duration(attempt))
		}
		sendErr = api.SendMessage(ctx, chatikID, intentID, text)
		if sendErr == nil {
			break
		}
		if !kit11.IsAmbiguous(sendErr) && !kit11.IsTransient(sendErr) {
			break // permanent: retrying will not help
		}
		// same-key retries are safe (docs/PLAN.md §6)
	}
	if sendErr == nil {
		if err := f.DB.IntentsMarkDone(ctx, intentID); err != nil {
			return sendFailed, err
		}
		return sendSent, nil
	}
	if kit11.IsAmbiguous(sendErr) {
		// Outcome unknown: the intent stays pending — nothing may repeat the
		// send; a human reconciles (review item makes this visible).
		f.reviewOnce(ctx, accountID, "send_ambiguous", "send outcome unknown — reconcile the chat",
			map[string]any{"chat": chatikID, "intent": intentKey},
			"sendamb:"+intentKey)
		return sendAmbiguous, sendErr
	}
	if err := f.DB.IntentsMarkFailed(ctx, intentID); err != nil {
		return sendFailed, err
	}
	if kit11.IsTransient(sendErr) {
		// known-outcome failure: nothing was sent; a later sync may retry
		// with a fresh key
		return sendFailed, sendErr
	}
	f.reviewOnce(ctx, accountID, "send_failed", "send rejected: "+sendErr.Error(),
		map[string]any{"chat": chatikID, "intent": intentKey},
		"sendfail:"+intentKey)
	return sendFailed, sendErr
}

// pace waits on the injected pacer before a mutation. A nil pacer is a
// no-op; a pacer error (typically ctx cancellation) aborts the mutation.
func (f *Feature) pace(ctx context.Context, accountID string) error {
	if f.Pacer == nil {
		return nil
	}
	if err := f.Pacer.Wait(ctx, accountID); err != nil {
		return fmt.Errorf("chatops: pacer wait: %w", err)
	}
	return nil
}

// textDelivered reconciles an ambiguous send by reading chat history.
func textDelivered(ctx context.Context, api hhapi.HHAPI, chatikID, text string) (bool, error) {
	hist, err := api.ChatHistory(ctx, chatikID)
	if err != nil {
		return false, err
	}
	norm := strings.Join(strings.Fields(text), " ")
	for _, m := range hist {
		if m.Direction == "out" && strings.Join(strings.Fields(m.Text), " ") == norm {
			return true, nil
		}
	}
	return false, nil
}

// --- state mirror + small helpers ---

// Chat state values (store CHECK-enforced).
const (
	stateActive    = "active"
	stateRejected  = "rejected"
	stateLeft      = "left"
	stateEscalated = "escalated"
	stateWarmupDue = "warmup_due"
)

// mirrorState is chatops' own persisted view of the chat state. The store
// chats.state is the source of truth; the mirror exists because the store
// contract exposes no single-chat read, and the sync pass needs the previous
// state to decide transitions (warmup_due→active on inbound, etc.).
func (f *Feature) mirrorState(ctx context.Context, accountID, chatikID string) string {
	val, ok, err := f.DB.FeatureStateGet(ctx, featureName, accountID, "state:"+chatikID)
	if err != nil || !ok || val == "" {
		if err := f.DB.FeatureStateSet(ctx, featureName, accountID, "state:"+chatikID, stateActive); err != nil {
			f.Log.Error("chatops: init mirror", "err", err)
		}
		return stateActive
	}
	return val
}

func (f *Feature) setMirror(ctx context.Context, accountID, chatikID, state string) string {
	if err := f.DB.FeatureStateSet(ctx, featureName, accountID, "state:"+chatikID, state); err != nil {
		f.Log.Error("chatops: update mirror", "err", err)
	}
	return state
}

func (f *Feature) leaveTries(ctx context.Context, accountID, chatikID string) (int, error) {
	val, _, err := f.DB.FeatureStateGet(ctx, featureName, accountID, "leavetry:"+chatikID)
	if err != nil || val == "" {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(val, "%d", &n); err != nil {
		return 0, nil
	}
	return n, nil
}

func (f *Feature) bumpLeaveTries(ctx context.Context, accountID, chatikID string) error {
	n, _ := f.leaveTries(ctx, accountID, chatikID)
	return f.DB.FeatureStateSet(ctx, featureName, accountID, "leavetry:"+chatikID, fmt.Sprint(n+1))
}

func (f *Feature) leaveDelay() time.Duration {
	if f.LeaveDelay != nil {
		return f.LeaveDelay()
	}
	return kit11.HumanizedDelay(leaveDelayMin, leaveDelayMax, f.Rnd)
}

// review records a review item (best-effort: review failures are logged, not
// propagated — they must never mask the decision that produced them).
func (f *Feature) review(ctx context.Context, accountID, kind, reason string, payload map[string]any) {
	p := mustJSON(payload)
	if f.Review != nil {
		// Wired reviewer: dedup of open items + needs_human webhook fan-out.
		if _, err := f.Review.Add(ctx, accountID, kind, reason, p); err != nil {
			f.Log.Error("chatops: review add", "kind", kind, "err", err)
		}
		return
	}
	if _, err := f.DB.ReviewItemsAdd(ctx, accountID, kind, reason, p); err != nil {
		f.Log.Error("chatops: review add", "kind", kind, "err", err)
	}
}

// reviewOnce records a review item only when no marker exists yet.
func (f *Feature) reviewOnce(ctx context.Context, accountID, kind, reason string, payload map[string]any, marker string) {
	if exists, _, err := f.DB.FeatureStateGet(ctx, featureName, accountID, "marker:"+marker); err == nil && exists != "" {
		return
	}
	_ = f.DB.FeatureStateSet(ctx, featureName, accountID, "marker:"+marker, "1")
	f.review(ctx, accountID, kind, reason, payload)
}

// artifactJSONWithAnswers builds a review/assessment payload for unscored tests.
func artifactJSONWithAnswers(answers []aiAnswerEntry, messageID string) string {
	return mustJSON(map[string]any{"message_id": messageID, "answers": answers})
}

func (f *Feature) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optI64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefI64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func strPtr(s string) *string { return &s }

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Feature)(nil)
