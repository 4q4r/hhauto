// Package captcha solves hh.ru challenges through paid API services only.
// Two protocol adapters cover 2captcha-compatible and anticaptcha-compatible
// services (base_url + key per provider); accounts hold ordered failover chains.
package captcha

import (
	"context"
	"errors"
	"time"
)

// Sentinel classification errors for the resolver.
var (
	ErrNoProviders = errors.New("captcha: no providers configured")
	ErrAllFailed   = errors.New("captcha: all providers failed")
	ErrUnsupported = errors.New("captcha: unsupported challenge type")
)

// Challenge types observed on hh.ru (verified in M0 spike; extendable).
const (
	TypeImage     = "image" // base64 picture captcha
	TypeHCaptcha  = "hcaptcha"
	TypeTurnstile = "turnstile"
)

// Challenge is a pending captcha task as reported by a provider.
type Challenge struct {
	Type     string // TypeImage | TypeHCaptcha | TypeTurnstile
	ImageB64 string // for TypeImage
	SiteKey  string // for hcaptcha/turnstile
	PageURL  string
}

// Solution is the provider answer for one Challenge.
type Solution struct {
	Token      string
	ProviderID string
	Latency    time.Duration
}

// Solver is one configured provider (protocol adapter + base_url + key).
type Solver interface {
	ID() string
	Solve(ctx context.Context, ch Challenge) (Solution, error)
	// Balance reports remaining service balance; used by `hhauto doctor`.
	Balance(ctx context.Context) (float64, error)
}

// Chain resolves challenges through an account's ordered provider list.
type Chain interface {
	Solve(ctx context.Context, ch Challenge) (Solution, error)
}
