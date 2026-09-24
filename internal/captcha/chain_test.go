package captcha

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSolver is an in-memory Solver for chain and resolver tests.
type fakeSolver struct {
	id      string
	sol     Solution
	err     error
	delay   time.Duration
	calls   int
	balance float64
	balErr  error
}

func (f *fakeSolver) ID() string { return f.id }

func (f *fakeSolver) Solve(ctx context.Context, _ Challenge) (Solution, error) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Solution{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Solution{}, f.err
	}
	return f.sol, nil
}

func (f *fakeSolver) Balance(_ context.Context) (float64, error) {
	if f.balErr != nil {
		return 0, f.balErr
	}
	return f.balance, nil
}

func TestChainReturnsFirstSuccessfulSolution(t *testing.T) {
	first := &fakeSolver{id: "a", sol: Solution{Token: "tok-a"}}
	chain := NewChain([]Solver{first}, nil)

	sol, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "x"})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	if sol.Token != "tok-a" {
		t.Fatalf("Solve() token = %q, want %q", sol.Token, "tok-a")
	}
	if sol.ProviderID != "a" {
		t.Fatalf("Solve() provider = %q, want %q", sol.ProviderID, "a")
	}
	if first.calls != 1 {
		t.Fatalf("first solver calls = %d, want 1", first.calls)
	}
}

func TestChainFailsOverAfterFirstError(t *testing.T) {
	first := &fakeSolver{id: "a", err: errors.New("boom")}
	second := &fakeSolver{id: "b", sol: Solution{Token: "tok-b"}}
	third := &fakeSolver{id: "c", sol: Solution{Token: "tok-c"}}

	chain := NewChain([]Solver{first, second, third}, nil)

	sol, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "x"})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	if sol.Token != "tok-b" {
		t.Fatalf("Solve() token = %q, want %q (failover must stop after first success)", sol.Token, "tok-b")
	}
	if first.calls != 1 || second.calls != 1 || third.calls != 0 {
		t.Fatalf("calls = a:%d b:%d c:%d, want a:1 b:1 c:0", first.calls, second.calls, third.calls)
	}
	if sol.ProviderID != "b" {
		t.Fatalf("Solve() provider = %q, want %q", sol.ProviderID, "b")
	}
}

func TestChainAllFailedWrapsErrAllFailed(t *testing.T) {
	first := &fakeSolver{id: "a", err: errors.New("boom-a")}
	second := &fakeSolver{id: "b", err: errors.New("boom-b")}

	chain := NewChain([]Solver{first, second}, nil)

	_, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "x"})
	if err == nil {
		t.Fatal("Solve() error = nil, want ErrAllFailed")
	}
	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("Solve() error = %v, want errors.Is(err, ErrAllFailed)", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = a:%d b:%d, want 1 and 1", first.calls, second.calls)
	}
}

func TestChainEmptyReturnsErrNoProviders(t *testing.T) {
	chain := NewChain(nil, nil)
	_, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "x"})
	if !errors.Is(err, ErrNoProviders) {
		t.Fatalf("Solve() error = %v, want ErrNoProviders", err)
	}
}

func TestChainProviderTimeoutSkipsToNext(t *testing.T) {
	slow := &fakeSolver{id: "slow", delay: 200 * time.Millisecond}
	fast := &fakeSolver{id: "fast", sol: Solution{Token: "tok-fast"}}

	chain := NewChain([]Solver{slow, fast}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	sol, err := chain.Solve(ctx, Challenge{Type: TypeImage, ImageB64: "x"})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}
	if sol.Token != "tok-fast" || sol.ProviderID != "fast" {
		t.Fatalf("Solve() = %+v, want token from fast provider", sol)
	}
}

func TestChainContextTimeoutFailsAll(t *testing.T) {
	slow := &fakeSolver{id: "slow-1", delay: 200 * time.Millisecond}
	slow2 := &fakeSolver{id: "slow-2", delay: 200 * time.Millisecond}

	chain := NewChain([]Solver{slow, slow2}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()

	_, err := chain.Solve(ctx, Challenge{Type: TypeImage, ImageB64: "x"})
	if err == nil {
		t.Fatal("Solve() error = nil, want ErrAllFailed")
	}
	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("Solve() error = %v, want errors.Is(err, ErrAllFailed)", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Solve() error = %v, want cause context.DeadlineExceeded", err)
	}
	if slow.calls != 1 || slow2.calls != 1 {
		t.Fatalf("calls = %d and %d, want both providers tried", slow.calls, slow2.calls)
	}
}

func TestChainEventsEmittedInOrder(t *testing.T) {
	var mu sync.Mutex
	var events []Event

	first := &fakeSolver{id: "a", err: errors.New("boom")}
	second := &fakeSolver{id: "b", sol: Solution{Token: "tok-b"}}
	chain := NewChain([]Solver{first, second}, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})

	_, err := chain.Solve(context.Background(), Challenge{Type: TypeImage, ImageB64: "x"})
	if err != nil {
		t.Fatalf("Solve() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("events = %d (%+v), want 2", len(events), events)
	}
	if events[0].ProviderID != "a" || events[0].OK {
		t.Fatalf("events[0] = %+v, want provider a with OK=false", events[0])
	}
	if events[1].ProviderID != "b" || !events[1].OK {
		t.Fatalf("events[1] = %+v, want provider b with OK=true", events[1])
	}
	for i, e := range events {
		if e.Kind != EventSolve {
			t.Fatalf("events[%d].Kind = %q, want %q", i, e.Kind, EventSolve)
		}
		if e.LatencyMS < 0 {
			t.Fatalf("events[%d].LatencyMS = %d, want >= 0", i, e.LatencyMS)
		}
	}
}
