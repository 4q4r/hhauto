package engine

import "testing"

func TestRedeliveryBook(t *testing.T) {
	var b redeliveryBook
	if got := b.next(1); got != 1 {
		t.Fatalf("first next = %d, want 1", got)
	}
	if got := b.next(1); got != 2 {
		t.Fatalf("second next = %d, want 2", got)
	}
	b.clear(1)
	if got := b.next(1); got != 1 {
		t.Fatalf("next after clear = %d, want 1 (reset on successful delivery)", got)
	}
	if got := b.next(2); got != 1 {
		t.Fatalf("independent ids: got %d, want 1", got)
	}
}
