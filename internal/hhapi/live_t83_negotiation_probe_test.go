//go:build live

package hhapi

// T83 live post-check: the vacancy that died with 400 test-required must
// now show a real negotiation topic (Мои отклики) — the solved-test apply
// landed. Read-only GET.
//
//	go test -tags live -run TestLiveT83Negotiation -v ./internal/hhapi/

import (
	"context"
	"testing"
	"time"
)

func TestLiveT83Negotiation(t *testing.T) {
	c := t83NewClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topics, err := c.Negotiations(ctx, 0)
	if err != nil {
		t.Fatalf("Negotiations: %v", err)
	}
	found := false
	for _, n := range topics {
		if n.VacancyID == t83VacancyID {
			found = true
			t.Logf("FOUND: id=%q vacancy=%d name=%q state=%q updatedAt=%s",
				n.ID, n.VacancyID, n.VacancyName, n.State, n.UpdatedAt)
		}
	}
	if !found {
		t.Fatalf("no negotiation topic for vacancy %d in %d topics", t83VacancyID, len(topics))
	}
}
