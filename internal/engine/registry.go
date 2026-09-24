package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"hhauto/internal/ai"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// TaskFactory builds the queue task for one schedule fire on one account.
// The payload shape is feature-owned; the engine only carries the task.
type TaskFactory func(ctx context.Context, accountID string) (queue.Task, error)

// TaskHandler executes one dequeued task. The engine has already checked
// the kill switch, session liveness and account pause; handlers call
// TaskContext.Pacer.Wait themselves right before a mutation.
type TaskHandler func(ctx context.Context, tc *TaskContext, task queue.Task) error

// TaskContext is everything a handler may need for one task. The pacer is
// deliberately NOT applied by the engine: only the handler knows which
// steps are mutations, so it waits via tc.Pacer before each one.
type TaskContext struct {
	DB       *store.DB
	API      hhapi.HHAPI
	AI       ai.Runner
	Pacer    Pacer
	Webhooks webhook.Notifier
	Captcha  captcha.Resolver
	Cfg      *config.Config
	Log      *slog.Logger

	AccountID string
}

// --- registration API (thread-safe) ---
//
// Features cannot import engine (dependency direction cli→engine→features),
// so the CLI wiring layer (T13) copies each enabled feature's schedule into
// engine.RegisterSchedule together with a TaskFactory over the feature's
// task constructors, and registers one TaskHandler per task kind. This
// package-level registry is process-global by design: features and wiring
// live in different packages.

type scheduleBinding struct {
	spec      scheduler.Spec
	factory   TaskFactory
	accountID string // "" = multi-account fan-out; non-empty = R11 single-account binding
}

var (
	regMu       sync.RWMutex
	scheduleReg = map[string]scheduleBinding{}
	handlerReg  = map[queue.TaskKind]TaskHandler{}
)

// RegisterSchedule binds a named schedule to its task factory. The spec
// must validate; re-registration replaces (idempotent wiring, test reloads).
//
// Deprecated (kept for compatibility): this is the multi-account fan-out
// binding — the schedule materializes one row PER configured account. New
// wiring should use RegisterScheduleForAccount (ruling R11).
func RegisterSchedule(name string, spec scheduler.Spec, factory TaskFactory) error {
	return registerSchedule(name, "", spec, factory)
}

// RegisterScheduleForAccount binds a named schedule to exactly one account
// (ruling R11): materialization creates exactly ONE schedule row for this
// name, and the factory receives exactly this accountID.
func RegisterScheduleForAccount(name, accountID string, spec scheduler.Spec, factory TaskFactory) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return fmt.Errorf("engine: RegisterScheduleForAccount %s: empty accountID", name)
	}
	return registerSchedule(name, accountID, spec, factory)
}

func registerSchedule(name, accountID string, spec scheduler.Spec, factory TaskFactory) error {
	if name == "" {
		return fmt.Errorf("engine: RegisterSchedule: empty name")
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("engine: RegisterSchedule %s: %w", name, err)
	}
	// Spec literals may omit the weekday mask; Spec.Next treats Days == 0
	// as "no day matches" and never fires. Normalize to every-day (the
	// same default scheduler.Parse applies) before the binding is stored —
	// both schedule materialization and next_run recomputation consume
	// this copy.
	if spec.Days == 0 {
		spec.Days = scheduler.All
	}
	if factory == nil {
		return fmt.Errorf("engine: RegisterSchedule %s: nil factory", name)
	}
	regMu.Lock()
	defer regMu.Unlock()
	scheduleReg[name] = scheduleBinding{spec: spec, factory: factory, accountID: accountID}
	return nil
}

// RegisterHandler binds a task kind to its executor. Re-registration
// replaces.
func RegisterHandler(kind queue.TaskKind, h TaskHandler) error {
	if kind == "" {
		return fmt.Errorf("engine: RegisterHandler: empty kind")
	}
	if h == nil {
		return fmt.Errorf("engine: RegisterHandler %s: nil handler", kind)
	}
	regMu.Lock()
	defer regMu.Unlock()
	handlerReg[kind] = h
	return nil
}

func registeredSchedules() map[string]scheduleBinding {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make(map[string]scheduleBinding, len(scheduleReg))
	for k, v := range scheduleReg {
		out[k] = v
	}
	return out
}

func registeredHandler(kind queue.TaskKind) (TaskHandler, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	h, ok := handlerReg[kind]
	return h, ok
}

// resetRegistrations clears both registries. Test isolation only: feature
// wiring happens once per process in production.
func resetRegistrations() {
	regMu.Lock()
	defer regMu.Unlock()
	scheduleReg = map[string]scheduleBinding{}
	handlerReg = map[queue.TaskKind]TaskHandler{}
}
