package setka

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hhauto/internal/config"
)

func fullEndpoints() config.SetkaEndpoints {
	return config.SetkaEndpoints{
		SearchPeople: "https://api.setka.ru/search",
		GetPerson:    "https://api.setka.ru/person",
		CanMessage:   "https://api.setka.ru/can_message",
		SendDM:       "https://api.setka.ru/send",
	}
}

// T61: the client constructor fails fast when any of the four Setka
// endpoints is unset — the live contracts are NOT captured, there are no
// defaults, and a half-configured client must not start.
func TestNewClientFailsFastOnMissingEndpoints(t *testing.T) {
	eps := fullEndpoints()
	for name := range epsAsMap(eps) {
		partial := fullEndpoints()
		switch name {
		case "search_people":
			partial.SearchPeople = ""
		case "get_person":
			partial.GetPerson = ""
		case "can_message":
			partial.CanMessage = ""
		case "send_dm":
			partial.SendDM = ""
		}
		_, err := New(partial)
		if err == nil {
			t.Fatalf("%s: expected fail-fast, got nil", name)
		}
		if !errors.Is(err, ErrEndpointNotCaptured) {
			t.Fatalf("%s: err = %v, want ErrEndpointNotCaptured", name, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: err must name the missing endpoint: %v", name, err)
		}
		if !strings.Contains(err.Error(), "живую разведку") {
			t.Fatalf("%s: err must point to live reconnaissance: %v", name, err)
		}
	}
	if _, err := New(fullEndpoints()); err != nil {
		t.Fatalf("full endpoints: %v", err)
	}
}

// T61: until the wire contracts are captured, every client method returns
// the loud not-captured error — never a silent skip, never fabricated data.
func TestClientMethodsFailLoud(t *testing.T) {
	c, err := New(fullEndpoints())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.SearchPeople(ctx, "Acme"); !errors.Is(err, ErrEndpointNotCaptured) {
		t.Fatalf("SearchPeople: %v", err)
	}
	if _, err := c.GetPerson(ctx, "u-1"); !errors.Is(err, ErrEndpointNotCaptured) {
		t.Fatalf("GetPerson: %v", err)
	}
	if _, err := c.CanMessage(ctx, "u-1"); !errors.Is(err, ErrEndpointNotCaptured) {
		t.Fatalf("CanMessage: %v", err)
	}
	if err := c.SendDM(ctx, "u-1", "привет"); !errors.Is(err, ErrEndpointNotCaptured) {
		t.Fatalf("SendDM: %v", err)
	}
}

func epsAsMap(e config.SetkaEndpoints) map[string]string {
	return map[string]string{
		"search_people": e.SearchPeople, "get_person": e.GetPerson,
		"can_message": e.CanMessage, "send_dm": e.SendDM,
	}
}
