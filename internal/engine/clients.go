package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"

	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/hhapi"
	"hhauto/internal/proxy"
	"hhauto/internal/tlsfp"
)

// cookiePersister adapts cookies.Store to the per-account hhapi.CookiePersister
// hook: every response's cookies are persisted under the owning account.
type cookiePersister struct {
	store cookies.Store
}

func (p cookiePersister) Save(ctx context.Context, accountID string, c []*http.Cookie) error {
	return p.store.Save(ctx, accountID, c)
}

// accountTransport resolves the transport wiring for one account:
// fail-closed proxy resolution straight from the same proxy table the
// registry was built from (an unknown ref is an error, never a direct
// fallback). The tls-client transport consumes the proxy as a URL
// (tls-client SetProxy), the legacy http transport as a raw dialer.
// loadBrowserSpec picks up the captured browser hello for the 1:1 TLS
// replay. Absent capture and stale/unparseable cache are WARN-level
// fallbacks to the preset profile; only a present, schema-current spec
// reaches the transport.
func loadBrowserSpec(cfg *config.Config, log *slog.Logger) *tlsfp.Spec {
	path := tlsfp.DefaultPath(cfg.General.DataDir)
	spec, err := tlsfp.Load(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("engine: captured browser TLS spec unusable; using the preset profile (run hhauto tls-probe)", "error", err, "path", path)
		} else {
			log.Warn("engine: no captured browser TLS spec; TLS fingerprint approximates the browser (run hhauto tls-probe)", "path", path)
		}
		return nil
	}
	log.Info("engine: replaying the captured browser ClientHello for hh connections",
		"browser_version", spec.BrowserVer, "captured_host", spec.Host, "ja3_md5", spec.JA3MD5)
	return spec
}

func accountTransport(cfg *config.Config, reg proxy.Registry, acct config.Account, mode, id string) (proxy.Dialer, []hhapi.ClientOption, error) {
	opts := []hhapi.ClientOption{hhapi.WithTransport(mode)}
	if acct.ProxyRef == "" {
		return nil, opts, nil
	}
	p, ok := cfg.Proxy[acct.ProxyRef]
	if !ok {
		return nil, nil, fmt.Errorf("engine: account %s: proxy_ref %q: unknown proxy", id, acct.ProxyRef)
	}
	switch mode {
	case config.TransportTLSClient:
		purl, err := proxy.URL(p)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: account %s: %w", id, err)
		}
		return nil, append(opts, hhapi.WithProxyURL(purl)), nil
	default: // config.TransportHTTP
		if reg == nil {
			return nil, nil, fmt.Errorf("engine: account %s: proxy_ref %q set but no proxy registry", id, acct.ProxyRef)
		}
		d, err := reg.Dialer(acct.ProxyRef) // unknown ref => error, never direct fallback
		if err != nil {
			return nil, nil, fmt.Errorf("engine: account %s: %w", id, err)
		}
		return d, opts, nil
	}
}

// BuildAPIClients constructs one hhapi client per configured account: the
// dialer comes from the proxy registry (empty proxy_ref means explicitly
// direct — the fail-closed decision stays with the registry), sessions are
// seeded from the cookie store and every response persists back into it.
// Unknown proxy references and missing user agents fail the whole build:
// a half-configured account must not run.
//
// TLS layer (T37 v3): when a captured browser hello exists under
// data_dir (hhauto tls-probe), every client replays it 1:1 instead of
// approximating the browser with a shipped preset. A missing or
// unusable capture degrades to the preset path with a WARN — it must
// never fail the accounts build.
func BuildAPIClients(ctx context.Context, cfg *config.Config, reg proxy.Registry, ckStore cookies.Store, log *slog.Logger) (map[string]hhapi.HHAPI, error) {
	if cfg == nil {
		return nil, fmt.Errorf("engine: nil config")
	}
	if log == nil {
		// CLI one-shots (account check, ai status) build clients without
		// a logger; the WARN/INFO paths below must not panic on it.
		log = slog.New(slog.DiscardHandler)
	}
	browserSpec := loadBrowserSpec(cfg, log)
	ids := make([]string, 0, len(cfg.Accounts))
	for id := range cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make(map[string]hhapi.HHAPI, len(ids))
	mode := cfg.API.Transport
	if mode == "" {
		mode = config.TransportTLSClient
	}
	for _, id := range ids {
		acct := cfg.Accounts[id]
		dialer, opts, err := accountTransport(cfg, reg, acct, mode, id)
		if err != nil {
			return nil, err
		}
		if browserSpec != nil {
			opts = append(opts, hhapi.WithBrowserTLSSpec(browserSpec))
		}
		client, err := hhapi.NewClient(acct, cfg.API.Endpoints, dialer, log, opts...)
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
