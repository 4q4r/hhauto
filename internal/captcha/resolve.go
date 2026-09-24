package captcha

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"hhauto/internal/config"
)

// Resolver produces per-account failover chains from the [captcha] provider
// table and each account's ordered captcha_chain.
type Resolver interface {
	// ChainFor returns the ordered failover chain for one account.
	// An account chain naming an unknown provider is a configuration error.
	ChainFor(accountID string) (Chain, error)
}

type resolver struct {
	solvers  map[string]Solver
	chainFor func(accountID string) []string
}

// New validates every configured captcha provider and wires the resolver.
// Supported protocols: "2captcha" (in.php/res.php) and "anticaptcha"
// (createTask/getTaskResult); base_url selects the concrete service clone.
//
// logger receives solver warnings (inline-key fallback and similar); pass
// the application's redaction-wrapped logger here. nil falls back to
// slog.Default() so tests and minimal embedders keep working.
//
// Provider-level problems (unknown protocol, missing key source, negative
// timings, malformed base_url) fail fast here; chain-level problems
// (unknown provider name, empty chain) surface from ChainFor.
func New(providers map[string]config.CaptchaProvider, chainFor func(accountID string) []string, logger *slog.Logger) (Resolver, error) {
	if len(providers) == 0 {
		return nil, ErrNoProviders
	}
	if chainFor == nil {
		return nil, fmt.Errorf("captcha: resolver requires a chainFor callback")
	}

	solvers := make(map[string]Solver, len(providers))
	for name, cfg := range providers {
		switch cfg.Protocol {
		case "2captcha":
			s, err := newTwoCaptcha(name, cfg, logger)
			if err != nil {
				return nil, err
			}
			solvers[name] = s
		case "anticaptcha":
			s, err := newAntiCaptcha(name, cfg, logger)
			if err != nil {
				return nil, err
			}
			solvers[name] = s
		default:
			return nil, fmt.Errorf("captcha: provider %q: unknown protocol %q (want 2captcha or anticaptcha)", name, cfg.Protocol)
		}
	}

	return &resolver{solvers: solvers, chainFor: chainFor}, nil
}

func (r *resolver) ChainFor(accountID string) (Chain, error) {
	names := r.chainFor(accountID)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: account %q: empty captcha_chain", ErrNoProviders, accountID)
	}

	providers := make([]Solver, 0, len(names))
	for _, name := range names {
		s, ok := r.solvers[name]
		if !ok {
			configured := make([]string, 0, len(r.solvers))
			for known := range r.solvers {
				configured = append(configured, known)
			}
			sort.Strings(configured)
			return nil, fmt.Errorf(
				"captcha: account %q: unknown provider %q in captcha_chain (configured: %s)",
				accountID, name, strings.Join(configured, ", "))
		}
		providers = append(providers, s)
	}

	// Wave 3 attaches a captcha_events sink here via NewChain's callback;
	// a nil callback keeps the resolver audit-free by default.
	return NewChain(providers, nil), nil
}
