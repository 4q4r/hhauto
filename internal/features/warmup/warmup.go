// Package warmup implements WF9 — silence pings ("автоподогрев", disabled by
// default).
//
// Division of labour: the chatops sync pass (which sees every chat on every
// poll) marks silent chats warmup_due and resets the ladder when the
// counterpart replies. This package consumes warmup_due chats: it walks the
// ladder (24/48/96h …), gates on the active window and weekend, produces the
// ping text from the configured list (rotating, avoiding the last N used) or
// from the AI, and sends it under a per-(chat,stage) idempotency intent.
// After MaxPings stages the chat is suppressed with a review flag.
package warmup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/kit11"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// FeatureName is the feature_state namespace shared by chatops (ladder
// resets) and this package.
const FeatureName = "warmup"

// rotationMemory: when picking a ping from the list, avoid the last N used.
const rotationMemory = 3

// ScanPayload is the queue payload for the periodic warmup scan.
type ScanPayload struct {
	AccountID string
}

// Feature consumes warmup_due chats for one deployment.
type Feature struct {
	Cfg    config.Config
	DB     *store.DB
	AI     ai.Runner
	API    kit11.APIResolver
	Notify webhook.Notifier
	// Pacer spaces the ping mutation. nil = no-op; the engine wiring
	// provides the account pacer (wave-3 review #4).
	Pacer featurekit.Pacer
	Log   *slog.Logger
	Now   func() time.Time
	Rnd   *rand.Rand
}

// Name implements features.Feature.
func (f *Feature) Name() string { return "warmup" }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.Cfg.Warmup.Enabled }

// RegisterTasks implements features.Feature: the scan runs on an hourly
// schedule (the ladder itself is driven by silence hours, so hourly polling
// is precise enough) plus one boot task per configured account.
func (f *Feature) RegisterTasks(ctx context.Context, q queue.Queue, reg *scheduler.Registry) error {
	times := make([]string, 0, 24)
	for h := range 24 {
		times = append(times, fmt.Sprintf("%02d:00", h))
	}
	reg.Add("warmup.scan", scheduler.Spec{TZ: f.Cfg.General.Timezone, Times: times})
	for accountID := range f.Cfg.Accounts {
		if err := q.Enqueue(ctx, queue.Task{
			AccountID: accountID,
			Kind:      queue.KindWarmupPing,
			Priority:  queue.PrioWarmup,
			DedupKey:  "warmup-boot",
			Payload:   ScanPayload{AccountID: accountID},
		}); err != nil {
			return fmt.Errorf("warmup: seed scan: %w", err)
		}
	}
	return nil
}

// HandleScan processes one scan task: ping every due warmup_due chat.
func (f *Feature) HandleScan(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(ScanPayload)
	if !ok {
		return fmt.Errorf("warmup: bad payload type %T", task.Payload)
	}
	if !f.Cfg.Warmup.Enabled {
		return nil
	}
	log := f.log()
	accountID := p.AccountID

	chats, err := f.DB.ChatsDueWarmup(ctx, accountID, 50)
	if err != nil {
		return fmt.Errorf("warmup: list chats: %w", err)
	}
	api, err := f.API(accountID)
	if err != nil {
		return fmt.Errorf("warmup: resolve api: %w", err)
	}

	for _, chat := range chats {
		if err := f.processChat(ctx, api, accountID, chat, log); err != nil {
			if kit11.IsTransient(err) {
				return err // abort scan; the scheduler retries
			}
			log.Error("warmup: chat processing failed", "chat", chat.ChatikChatID, "err", err)
		}
	}
	return nil
}

// processChat runs the full ladder/decision logic for one chat.
func (f *Feature) processChat(ctx context.Context, api hhapi.HHAPI, accountID string, chat store.Chat, log *slog.Logger) error {
	now := f.now()
	ladder, err := LadderOf(ctx, f.DB, accountID, chat.ChatikChatID)
	if err != nil {
		return err
	}

	// Ladder exhausted → suppress once, then stay quiet.
	if ladder >= f.Cfg.Warmup.MaxPings {
		return f.suppressOnce(ctx, accountID, chat)
	}

	// Stage due? LadderHours[stage-1] hours after the last outgoing message.
	if chat.LastOutgoingAt == nil || chat.LastOutgoingAt.IsZero() {
		return nil
	}
	stage := ladder + 1
	due := *chat.LastOutgoingAt
	if delay, ok := ladderDelay(f.Cfg.Warmup.LadderHours, ladder); ok {
		due = due.Add(delay)
	} else {
		due = due.Add(24 * time.Hour)
	}
	if now.Before(due) {
		return nil
	}

	// Human-hours gates: active window and weekend.
	if !kit11.InActiveWindow(now.Local(), f.Cfg.Warmup.ActiveHours) {
		return nil
	}
	respectWeekend := f.Cfg.Warmup.RespectWeekend == nil || *f.Cfg.Warmup.RespectWeekend
	if respectWeekend && kit11.IsWeekend(now.Local()) {
		return nil
	}

	text, err := f.pingText(ctx, accountID, chat, now)
	if err != nil {
		return err
	}
	if kit11.HasLink(text) {
		return f.review(ctx, accountID, "warmup_ping_links", chat.ChatikChatID,
			"warmup: ping text contains a link (forbidden)")
	}

	stageKey := fmt.Sprintf("ping:%s:%d", chat.ChatikChatID, stage)
	intentID, err := f.DB.IntentsBegin(ctx, stageKey, "pending", text)
	switch {
	case errors.Is(err, store.ErrIntentDone):
		return nil // this stage was already sent
	case errors.Is(err, store.ErrIntentPending):
		return nil // another attempt is in flight
	case err != nil:
		return fmt.Errorf("warmup: intent: %w", err)
	}

	if f.Cfg.General.DryRun {
		// Wave-3 review #7: a dry-run must not leave a pending intent —
		// that would block the real send of this stage forever.
		_ = f.DB.IntentsMarkAbandoned(ctx, intentID)
		log.Info("warmup: dry-run, ping skipped", "chat", chat.ChatikChatID, "stage", stage, "text", text)
		return nil
	}

	// One ping_queue row per chat (UNIQUE chat_id+attempts): a ledger entry,
	// not the ladder driver. Already present → the chat was pinged before.
	// Scheduled only on the real-send path so a dry-run leaves no row.
	if _, scheduled, err := f.DB.PingQueueSchedule(ctx, accountID, chat.ID, now, 1); err != nil {
		return fmt.Errorf("warmup: schedule ping: %w", err)
	} else if !scheduled && ladder == 0 {
		// A row exists but the ladder says stage 1: a previous run already
		// scheduled/sent a ping that this ladder lost track of — do not
		// double-ping, repair the ladder instead.
		_ = f.DB.IntentsMarkAbandoned(ctx, intentID)
		return nil
	}

	// Pacer before the ping mutation (wave-3 review #4).
	if f.Pacer != nil {
		if err := f.Pacer.Wait(ctx, accountID); err != nil {
			_ = f.DB.IntentsMarkFailed(ctx, intentID)
			return fmt.Errorf("warmup: pacer: %w", err)
		}
	}

	sendErr := api.SendMessage(ctx, chat.ChatikChatID, intentID, text)
	if sendErr == nil {
		return f.completePing(ctx, accountID, chat, intentID, stage, text, now, log)
	}
	if kit11.IsAmbiguous(sendErr) {
		// Outcome unknown — RECONCILE by reading state, never blind repeat.
		sent, rerr := pingDelivered(ctx, api, chat.ChatikChatID, text)
		if rerr != nil {
			// Cannot reconcile: leave the intent pending (blocks any repeat)
			// and surface for a human.
			if rerr2 := f.review(ctx, accountID, "warmup_send_ambiguous", chat.ChatikChatID,
				"warmup: ping outcome unknown and chat unreadable — check manually"); rerr2 != nil {
				return rerr2
			}
			return sendErr
		}
		if sent {
			return f.completePing(ctx, accountID, chat, intentID, stage, text, now, log)
		}
		if err := f.DB.IntentsMarkFailed(ctx, intentID); err != nil {
			return err
		}
		return f.review(ctx, accountID, "warmup_send_ambiguous", chat.ChatikChatID,
			"warmup: ping send outcome unknown, not delivered — will not repeat automatically")
	}
	if kit11.IsTransient(sendErr) {
		if err := f.DB.IntentsMarkFailed(ctx, intentID); err != nil {
			return err
		}
		return sendErr // transient: abort scan, scheduler retries later
	}
	if err := f.DB.IntentsMarkFailed(ctx, intentID); err != nil {
		return err
	}
	return f.review(ctx, accountID, "warmup_send_failed", chat.ChatikChatID,
		"warmup: ping rejected: "+sendErr.Error())
}

// completePing finishes a confirmed ping: done-intent, sent marker, ladder.
func (f *Feature) completePing(ctx context.Context, accountID string, chat store.Chat, intentID string, stage int, text string, now time.Time, log *slog.Logger) error {
	if err := f.DB.IntentsMarkDone(ctx, intentID); err != nil {
		return err
	}
	if err := f.DB.PingQueueMarkSent(ctx, chat.ID); err != nil {
		return err
	}
	if err := SetLadder(ctx, f.DB, accountID, chat.ChatikChatID, stage); err != nil {
		return err
	}
	if err := f.rememberUsed(ctx, accountID, chat.ChatikChatID, text); err != nil {
		return err
	}
	if err := f.DB.MetricsIncr(ctx, "warmup.ping", kit11.Day(now)); err != nil {
		return err
	}
	log.Info("warmup: ping sent", "chat", chat.ChatikChatID, "stage", stage)
	return nil
}

// pingDelivered reconciles an ambiguous ping by reading the chat history:
// the ping is "delivered" when our exact text appears as an outbound message.
func pingDelivered(ctx context.Context, api hhapi.HHAPI, chatikID, text string) (bool, error) {
	hist, err := api.ChatHistory(ctx, chatikID)
	if err != nil {
		return false, err
	}
	for _, m := range hist {
		if m.Direction == "out" && strings.TrimSpace(m.Text) == strings.TrimSpace(text) {
			return true, nil
		}
	}
	return false, nil
}

// pingText produces the ping text from the configured source.
func (f *Feature) pingText(ctx context.Context, accountID string, chat store.Chat, now time.Time) (string, error) {
	switch f.Cfg.Warmup.Source {
	case "ai":
		return f.aiPingText(ctx, accountID, chat, now)
	default: // "list" and anything unset
		return f.listPingText(ctx, accountID, chat)
	}
}

// listPingText rotates through PingList avoiding the last rotationMemory used.
func (f *Feature) listPingText(ctx context.Context, accountID string, chat store.Chat) (string, error) {
	list := f.Cfg.Warmup.PingList
	if len(list) == 0 {
		return f.aiPingText(ctx, accountID, chat, f.now())
	}
	used := f.usedIndexes(ctx, accountID, chat.ChatikChatID, len(list))
	avoid := map[int]bool{}
	for _, idx := range used[max(0, len(used)-rotationMemory):] {
		avoid[idx] = true
	}
	for idx := range list {
		if !avoid[idx] {
			return list[idx], nil
		}
	}
	// All recently used (tiny list): rotate deterministically by ladder.
	ladder, _ := LadderOf(ctx, f.DB, accountID, chat.ChatikChatID)
	return list[ladder%len(list)], nil
}

// aiPingText asks the AI for a warmup ping.
func (f *Feature) aiPingText(ctx context.Context, accountID string, chat store.Chat, now time.Time) (string, error) {
	if f.AI == nil {
		return "", errors.New("warmup: source=ai but no AI runner configured")
	}
	hours := 0
	if chat.LastOutgoingAt != nil {
		hours = int(now.Sub(*chat.LastOutgoingAt).Hours())
		if hours < 0 {
			hours = 0
		}
	}
	input := map[string]any{
		"hours_silent": hours,
		"topic":        "статус рассмотрения отклика",
		"template":     "",
	}
	raw, _, err := f.AI.Run(ctx, accountID, "chat", chat.ChatikChatID, "", ai.TaskWarmupPing, input)
	if err != nil {
		return "", fmt.Errorf("warmup: ai ping: %w", err)
	}
	var verdict struct {
		ai.Common
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return "", fmt.Errorf("warmup: ai verdict decode: %w", err)
	}
	if verdict.NeedsHuman || verdict.InjectionSuspected || strings.TrimSpace(verdict.Text) == "" {
		if err := f.review(ctx, accountID, "warmup_unsure", chat.ChatikChatID,
			"warmup: AI declined to draft a ping"); err != nil {
			return "", err
		}
		return "", errors.New("warmup: AI unsure, ping skipped")
	}
	return verdict.Text, nil
}

// rememberUsed appends the chosen list index to the rotation memory.
func (f *Feature) rememberUsed(ctx context.Context, accountID, chatikID, text string) error {
	list := f.Cfg.Warmup.PingList
	for idx, item := range list {
		if item == text {
			used := f.usedIndexes(ctx, accountID, chatikID, len(list))
			used = append(used, idx)
			b, err := json.Marshal(used)
			if err != nil {
				return fmt.Errorf("warmup: rotation marshal: %w", err)
			}
			return f.DB.FeatureStateSet(ctx, FeatureName, accountID, "used:"+chatikID, string(b))
		}
	}
	return nil
}

func (f *Feature) usedIndexes(ctx context.Context, accountID, chatikID string, listLen int) []int {
	val, ok, err := f.DB.FeatureStateGet(ctx, FeatureName, accountID, "used:"+chatikID)
	if err != nil || !ok || val == "" {
		return nil
	}
	var used []int
	if json.Unmarshal([]byte(val), &used) != nil {
		return nil
	}
	// Drop indexes that no longer fit the (possibly changed) list.
	out := used[:0]
	for _, idx := range used {
		if idx >= 0 && idx < listLen {
			out = append(out, idx)
		}
	}
	return out
}

// suppressOnce records the ladder-exhausted review exactly once.
func (f *Feature) suppressOnce(ctx context.Context, accountID string, chat store.Chat) error {
	if _, ok, err := f.DB.FeatureStateGet(ctx, FeatureName, accountID, "suppressed:"+chat.ChatikChatID); err != nil {
		return err
	} else if ok {
		return nil
	}
	if err := f.review(ctx, accountID, "warmup_suppressed", chat.ChatikChatID,
		fmt.Sprintf("warmup: %d pings sent with no reply — suppressed", f.Cfg.Warmup.MaxPings)); err != nil {
		return err
	}
	return f.DB.FeatureStateSet(ctx, FeatureName, accountID, "suppressed:"+chat.ChatikChatID, "1")
}

func (f *Feature) review(ctx context.Context, accountID, kind, chatikID, reason string) error {
	if _, err := f.DB.ReviewItemsAdd(ctx, accountID, kind, reason,
		mustJSON(map[string]any{"chat": chatikID})); err != nil {
		return fmt.Errorf("warmup: review add: %w", err)
	}
	if f.Notify != nil {
		f.Notify.Emit(webhook.Event{
			Type:      webhook.EvNeedsHuman,
			AccountID: accountID,
			Time:      f.now(),
			Message:   reason,
			Payload:   map[string]any{"chat": chatikID, "kind": kind},
		})
	}
	return nil
}

// LadderOf returns the current ladder stage (0 = no ping sent yet).
func LadderOf(ctx context.Context, db *store.DB, accountID, chatikID string) (int, error) {
	val, ok, err := db.FeatureStateGet(ctx, FeatureName, accountID, "ladder:"+chatikID)
	if err != nil {
		return 0, fmt.Errorf("warmup: ladder read: %w", err)
	}
	if !ok || val == "" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscanf(val, "%d", &n); err != nil {
		return 0, nil
	}
	if n < 0 {
		n = 0
	}
	return n, nil
}

// SetLadder persists the ladder stage; ResetLadder sets it to 0 (new inbound
// message re-engaged the counterpart).
func SetLadder(ctx context.Context, db *store.DB, accountID, chatikID string, stage int) error {
	return db.FeatureStateSet(ctx, FeatureName, accountID, "ladder:"+chatikID, fmt.Sprint(stage))
}

// ResetLadder rewinds the warmup ping ladder to zero for a chat (used when
// the account answers and the warmup cycle restarts).
func ResetLadder(ctx context.Context, db *store.DB, accountID, chatikID string) error {
	return SetLadder(ctx, db, accountID, chatikID, 0)
}

// ladderDelay returns the silence duration before stage stageNo+1.
func ladderDelay(ladderHours []int, stageNo int) (time.Duration, bool) {
	if len(ladderHours) == 0 {
		return 0, false
	}
	if stageNo >= len(ladderHours) {
		stageNo = len(ladderHours) - 1
	}
	if stageNo < 0 {
		return 0, false
	}
	return time.Duration(ladderHours[stageNo]) * time.Hour, true
}

func (f *Feature) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

func (f *Feature) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Feature)(nil)
