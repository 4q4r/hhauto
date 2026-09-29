// Package setka is the Setka (setka.ru / api.setka.ru) client surface: the
// SetkaClient contract the post-apply DM feature consumes plus a skeleton
// implementation that follows the repo's hh patterns (cookie session, uTLS
// transport, XSRF-analog) WITHOUT invented endpoints.
//
// IRON CONSTRAINT (T61): the live Setka wire contracts are not captured yet
// (see .sdd/research-setka.md §6 — the reconnaissance checklist). The
// endpoint URLs come ONLY from config overrides ([features.setka.endpoints]);
// an unset mandatory one is a loud fail-fast ("эндпоинт не захвачен —
// прогони живую разведку"), never a silent skip. T64: can_message has no
// wire endpoint at all (t63-setka-recon.md §4) and is optional — empty
// means every profile is considered writable. The methods below stay typed
// stubs until
// the live capture lands: they return ErrEndpointNotCaptured instead of
// fabricating request/response shapes. Everything above the transport is
// real and tested against fakes.
package setka

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"hhauto/internal/config"
)

// ErrEndpointNotCaptured marks a Setka surface the live reconnaissance has
// not captured yet: either an unset config endpoint (startup fail-fast) or
// an uncaptured wire contract (method call). Loud by design.
var ErrEndpointNotCaptured = errors.New("эндпоинт Сетки не захвачен — прогони живую разведку (research-setka.md §6)")

// ErrAccountNotActivated marks a SendDM refusal for a profile whose Setka
// account is not activated — a "ghost" imported from hh that can never be
// written to (the API answers with the ACCOUNT_NOT_ACTIVATED code,
// t63-setka-recon.md §3.2). The feature turns it into a permanent skip.
var ErrAccountNotActivated = errors.New("ACCOUNT_NOT_ACTIVATED: аккаунт Сетки не активирован («призрак» из hh)")

// Person is one Setka profile as the feature consumes it. SearchPeople
// returns the light card (ID, Name, Headline, Company, Status); GetPerson
// fills the full ProfileText for personalization. CanMessage marks a
// writable profile — Setka "ghosts" (imported from hh, never registered)
// cannot be written to.
type Person struct {
	ID          string
	Name        string
	Headline    string
	Company     string
	Status      string
	CanMessage  bool
	ProfileText string
}

// SetkaClient is the transport contract of the Setka feature: search a
// company's people, read one profile, check writability, send a direct
// message. The wire implementation waits for the live capture; tests run on
// fakes of this interface. (The stuttering name is the T61 contract term.)
//
//nolint:revive // «SetkaClient» — имя из контракта T61, не переименовывать
type SetkaClient interface {
	SearchPeople(ctx context.Context, company string) ([]Person, error)
	GetPerson(ctx context.Context, personID string) (*Person, error)
	CanMessage(ctx context.Context, personID string) (bool, error)
	SendDM(ctx context.Context, personID, text string) error
}

// Client is the skeleton SetkaClient: endpoint URLs from config only,
// cookie-session + uTLS transport + XSRF-analog to be wired by the live
// capture task (the repo's hhapi patterns are the blueprint).
type Client struct {
	eps config.SetkaEndpoints
}

// New validates the configured endpoints up front: the three mandatory
// ones must be set (there are no defaults to invent) — otherwise it fails
// fast with the missing names in the message. T64: can_message is optional
// (the wire has no such endpoint — t63-setka-recon.md §4); empty means the
// owner policy "everyone is writable".
func New(eps config.SetkaEndpoints) (*Client, error) {
	var missing []string
	for name, raw := range map[string]string{
		"search_people": eps.SearchPeople,
		"get_person":    eps.GetPerson,
		"send_dm":       eps.SendDM,
	} {
		if strings.TrimSpace(raw) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: не заданы: %s", ErrEndpointNotCaptured, strings.Join(missing, ", "))
	}
	return &Client{eps: eps}, nil
}

// SearchPeople implements SetkaClient. Stub until the live capture.
func (c *Client) SearchPeople(_ context.Context, _ string) ([]Person, error) {
	return nil, fmt.Errorf("setka: SearchPeople (%s): %w", c.eps.SearchPeople, ErrEndpointNotCaptured)
}

// GetPerson implements SetkaClient. Stub until the live capture.
func (c *Client) GetPerson(_ context.Context, _ string) (*Person, error) {
	return nil, fmt.Errorf("setka: GetPerson (%s): %w", c.eps.GetPerson, ErrEndpointNotCaptured)
}

// CanMessage implements SetkaClient. T64: the wire has no can_message
// endpoint (t63-setka-recon.md §4) — an unset endpoint means every profile
// is considered writable; a configured one still waits for the live
// capture.
func (c *Client) CanMessage(_ context.Context, _ string) (bool, error) {
	if strings.TrimSpace(c.eps.CanMessage) == "" {
		return true, nil
	}
	return false, fmt.Errorf("setka: CanMessage (%s): %w", c.eps.CanMessage, ErrEndpointNotCaptured)
}

// SendDM implements SetkaClient. Stub until the live capture.
func (c *Client) SendDM(_ context.Context, _, _ string) error {
	return fmt.Errorf("setka: SendDM (%s): %w", c.eps.SendDM, ErrEndpointNotCaptured)
}

// Compile-time conformance with the transport contract.
var _ SetkaClient = (*Client)(nil)
