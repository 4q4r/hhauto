package ai

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// testLogger discards output; individual tests can assert on provider
// behavior instead of log text.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeLedger records decisions and serves a configurable day total for
// budget checks; dayTokens lets tests derive the total from recorded recs.
type fakeLedger struct {
	mu        sync.Mutex
	recs      []*DecisionRecord
	tokens    int
	cost      float64
	addErr    error
	dayTokens func(l *fakeLedger) (int, float64)
}

func (l *fakeLedger) Add(rec *DecisionRecord) error {
	if l.addErr != nil {
		return l.addErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	clone := *rec
	l.recs = append(l.recs, &clone)
	return nil
}

func (l *fakeLedger) SumTokensDay(day string) (int, float64) {
	if l.dayTokens != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.dayTokens(l)
	}
	return l.tokens, l.cost
}

// scriptedProvider returns canned model replies, then keeps repeating the
// last one; err is returned instead when set. The last captured request is
// kept for system/user-block assertions.
type scriptedProvider struct {
	mu       sync.Mutex
	contents []string
	err      error
	calls    int
	dialect  string
	lastReq  *Request
	lastSys  string
	lastUser string
}

func (s *scriptedProvider) Complete(ctx context.Context, req Request) (Response, error) {
	s.mu.Lock()
	s.calls++
	saved := req
	s.lastReq = &saved
	if len(req.Messages) > 0 {
		s.lastSys = req.System
		s.lastUser = req.Messages[0].Content
	}
	s.mu.Unlock()
	if s.err != nil {
		return Response{}, s.err
	}
	content := ""
	if len(s.contents) > 0 {
		if s.calls > len(s.contents) {
			content = s.contents[len(s.contents)-1]
		} else {
			content = s.contents[s.calls-1]
		}
	}
	return Response{Content: content, Model: "scripted", TokensIn: 10, TokensOut: 5, LatencyMS: 3}, nil
}

func (s *scriptedProvider) captured() (sys, user string, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSys, s.lastUser, s.calls
}

func (s *scriptedProvider) Dialect() string {
	if s.dialect == "" {
		return "scripted"
	}
	return s.dialect
}

// completeModel lets NewBoundProvider bind the fake, mirroring real adapters
// (the bound model name is echoed back in Response.Model).
func (s *scriptedProvider) completeModel(ctx context.Context, model string, req Request) (Response, error) {
	resp, err := s.Complete(ctx, req)
	resp.Model = model
	return resp, err
}

func (s *scriptedProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
