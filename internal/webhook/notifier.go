package webhook

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

// notifyQueueCapacity bounds the in-memory event queue. Events emitted while
// the queue is full are dropped (counted and logged) so Emit never blocks.
const notifyQueueCapacity = 1024

// notifier fans events out to all configured endpoints from a single worker,
// keeping per-notifier delivery order while never blocking the caller.
type notifier struct {
	endpoints []*endpoint
	outbox    Outbox
	log       *slog.Logger

	ch      chan Event
	done    chan struct{}
	baseCtx context.Context
	cancel  context.CancelFunc

	emitMu    sync.Mutex // serializes Emit sends against channel close
	closed    bool
	closeOnce sync.Once

	dropped atomic.Int64
}

var _ Notifier = (*notifier)(nil)

// New builds a notifier for the given endpoints. Endpoints with
// configuration errors (unknown preset, missing secret env, unresolvable
// proxy_ref) are disabled with an error log instead of failing startup. A
// nil outbox keeps failed deliveries in memory only; a nil logger falls
// back to slog.Default.
func New(endpoints []config.Webhook, proxyReg proxy.Registry, outbox Outbox, log *slog.Logger) *notifier {
	if log == nil {
		log = slog.Default()
	}
	n := &notifier{
		outbox: outbox,
		log:    log,
		ch:     make(chan Event, notifyQueueCapacity),
		done:   make(chan struct{}),
	}
	n.baseCtx, n.cancel = context.WithCancel(context.Background())

	for _, cfg := range endpoints {
		ep, err := newEndpoint(cfg, proxyReg)
		if err != nil {
			log.Error("webhook endpoint disabled: configuration error", "err", err)
			continue
		}
		n.endpoints = append(n.endpoints, ep)
	}

	if len(n.endpoints) > 0 {
		go n.run()
	} else {
		close(n.done) // nothing to deliver: Close must not wait for a worker
	}
	return n
}

// Emit queues an event for delivery. It never blocks: when the queue is full
// the event is dropped, counted, and logged at WARN.
func (n *notifier) Emit(ev Event) {
	n.emitMu.Lock()
	defer n.emitMu.Unlock()
	if n.closed {
		n.log.Warn("webhook event dropped: notifier is closed", "event", ev.Type)
		return
	}
	select {
	case n.ch <- ev:
	default:
		total := n.dropped.Add(1)
		n.log.Warn("webhook queue full: event dropped", "event", ev.Type, "dropped_total", total)
	}
}

// Dropped returns how many events were dropped due to a full queue.
func (n *notifier) Dropped() int64 { return n.dropped.Load() }

// Close stops accepting events and waits for the worker to drain the queue.
// If ctx expires first, in-flight deliveries are cancelled, the worker is
// joined, and ctx.Err() is returned.
func (n *notifier) Close(ctx context.Context) error {
	n.closeOnce.Do(func() {
		n.emitMu.Lock()
		n.closed = true
		n.emitMu.Unlock()
		close(n.ch)
	})
	select {
	case <-n.done:
		n.cancel()
		return nil
	case <-ctx.Done():
		n.cancel() // abort in-flight HTTP attempts; the worker then exits
		<-n.done
		return ctx.Err()
	}
}

func (n *notifier) run() {
	defer close(n.done)
	for ev := range n.ch {
		n.dispatch(n.baseCtx, ev)
	}
}

// dispatch delivers one event to every subscribed endpoint; exhausted
// deliveries are parked in the outbox (if any) for later redelivery.
func (n *notifier) dispatch(ctx context.Context, ev Event) {
	for _, ep := range n.endpoints {
		if !ep.subscribes(ev.Type) {
			continue
		}
		payload, err := ep.deliver(ctx, ev)
		if err != nil {
			n.log.Error("webhook delivery failed", "endpoint", ep.id, "event", ev.Type, "err", err)
			if parkErr := ep.park(ctx, payload, n.outbox); parkErr != nil {
				n.log.Error("webhook outbox Add failed", "endpoint", ep.id, "event", ev.Type, "err", parkErr)
			}
		}
	}
}
