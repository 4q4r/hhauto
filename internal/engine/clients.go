package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/hhapi"
	"hhauto/internal/proxy"
)

// cookiePersister adapts cookies.Store to the per-account hhapi.CookiePersister
// hook: every response's cookies are persisted under the owning account.
type cookiePersister struct {
	store cookies.Store
}

func (p cookiePersister) Save(ctx context.Context, accountID string, c []*http.Cookie) error {
	return p.store.Save(ctx, accountID, c)
}

// BuildAPIClients constructs one hhapi client per configured account: the
// dialer comes from the proxy registry (empty proxy_ref means explicitly
// direct — the fail-closed decision stays with the registry), sessions are
// seeded from the cookie store and every response persists back into it.
// Unknown proxy references and missing user agents fail the whole build:
// a half-configured account must not run.
func BuildAPIClients(ctx context.Context, cfg *config.Config, reg proxy.Registry, ckStore cookies.Store, log *slog.Logger) (map[string]hhapi.HHAPI, error) {
	if cfg == nil {
		return nil, fmt.Errorf("engine: nil config")
	}
	ids := make([]string, 0, len(cfg.Accounts))
	for id := range cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make(map[string]hhapi.HHAPI, len(ids))
	for _, id := range ids {
		acct := cfg.Accounts[id]
		var dialer proxy.Dialer
		if acct.ProxyRef != "" {
			if reg == nil {
				return nil, fmt.Errorf("engine: account %s: proxy_ref %q set but no proxy registry", id, acct.ProxyRef)
			}
			d, err := reg.Dialer(acct.ProxyRef) // unknown ref => error, never direct fallback
			if err != nil {
				return nil, fmt.Errorf("engine: account %s: %w", id, err)
			}
			dialer = d
		}
		client, err := hhapi.NewClient(acct, cfg.API.Endpoints, dialer, log)
		if err != nil {
			return nil, fmt.Errorf("engine: account %s: %w", id, err)
		}
		if ckStore != nil {
			client.SetPersister(cookiePersister{store: ckStore}, id)
			cks, err := ckStore.Load(ctx, id)
			switch err {
			case nil:
				if lerr := client.LoadCookies(ctx, cks); lerr != nil {
					return nil, fmt.Errorf("engine: account %s: load cookies: %w", id, lerr)
				}
			default:
				if !errors.Is(err, cookies.ErrNotFound) {
					return nil, fmt.Errorf("engine: account %s: load cookies: %w", id, err)
				}
				// no saved session yet: fresh login flow
			}
		}
		out[id] = client
	}
	return out, nil
}

// flushCookies persists each account's current session snapshot once at
// shutdown. Regular persistence already happens per response via the
// persister; this is the belt-and-braces final flush of the graceful path.
func flushCookies(ctx context.Context, ckStore cookies.Store, apis map[string]hhapi.HHAPI, log *slog.Logger) {
	if ckStore == nil {
		return
	}
	for id, api := range apis {
		snapper, ok := api.(interface{ Cookies() []*http.Cookie })
		if !ok {
			continue
		}
		cks := snapper.Cookies()
		if len(cks) == 0 {
			continue
		}
		if err := ckStore.Save(ctx, id, cks); err != nil {
			log.Warn("engine: cookie flush failed", "account", id, "err", err)
		}
	}
}
