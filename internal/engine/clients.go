package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"

	"hhauto/internal/browser"
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

// captureTLSSpec is the automatic capture seam (overridden in tests):
// the same capture the tls-probe command runs, from the [browser] config.
var captureTLSSpec = func(ctx context.Context, cfg *config.Config, log *slog.Logger) (*tlsfp.Spec, error) {
	return browser.CaptureTLSSpec(ctx, cfg, "", log)
}

// loadBrowserSpec returns the captured browser TLS spec — strictly (T50):
// an absent or unusable capture is recaptured automatically through the
// same capture the tls-probe command runs, so the engine only ever works
// with a 1:1 spec. A failed capture fails the build loudly; the preset
// (approximated) profile path no longer exists.
func loadBrowserSpec(ctx context.Context, cfg *config.Config, log *slog.Logger) (*tlsfp.Spec, error) {
	path := tlsfp.DefaultPath(cfg.General.DataDir)
	spec, err := tlsfp.Load(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Info("engine: no captured browser TLS spec; capturing it automatically", "path", path)
		} else {
			log.Info("engine: captured browser TLS spec unusable; capturing a fresh one", "error", err, "path", path)
		}
		spec, err = captureTLSSpec(ctx, cfg, log)
		if err != nil {
			return nil, fmt.Errorf("engine: automatic TLS capture failed (install a Chrome/Chromium binary or enable browser.auto_download; hhauto tls-probe reruns the same capture): %w", err)
		}
	}
	log.Info("engine: replaying the captured browser ClientHello for hh connections",
		"browser_version", spec.BrowserVer, "captured_host", spec.Host, "ja3_md5", spec.JA3MD5)
	return spec, nil
}

// resolveUserAgent returns the effective account User-Agent (T50): the
// captured browser UA wins — the spec is the single source of truth and
// JA3 plus UA must describe the same browser. A difference is routine
// (INFO), not a mismatch to warn about: with a 1:1 spec the captured UA
// is by definition the right one.
func resolveUserAgent(acct config.Account, spec *tlsfp.Spec) (string, bool) {
	if spec == nil || spec.UserAgent == "" || spec.UserAgent == acct.UserAgent {
		return acct.UserAgent, false
	}
	return spec.UserAgent, true
}

// accountTransport resolves the transport wiring for one account:
// fail-closed proxy resolution straight from the same proxy table the
// registry was built from (an unknown ref is an error, never a direct
// fallback). The tls-client transport consumes the proxy as a URL
// (tls-client SetProxy), the legacy http transport as a raw dialer.
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
// TLS layer (T50, strict parity): the captured browser hello is
// mandatory — a missing or unusable capture is recaptured automatically
// (the same capture the tls-probe command runs) and a failed capture
// fails the build. Every account runs the captured spec 1:1 and sends
// the captured browser User-Agent; there is no preset-profile fallback.
func BuildAPIClients(ctx context.Context, cfg *config.Config, reg proxy.Registry, ckStore cookies.Store, log *slog.Logger) (map[string]hhapi.HHAPI, error) {
	if cfg == nil {
		return nil, fmt.Errorf("engine: nil config")
	}
	if log == nil {
		// CLI one-shots (account check, ai status, onboarding) build
		// clients without a logger. T50: the capture/replay decisions
		// must stay visible, so nil means the default stderr logger —
		// not a discard handler.
		log = slog.Default()
	}
	browserSpec, err := loadBrowserSpec(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
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
		if ua, overridden := resolveUserAgent(acct, browserSpec); overridden {
			log.Info("engine: using the captured browser User-Agent",
				"account", id, "configured", acct.UserAgent, "captured", ua)
			acct.UserAgent = ua
		}
		dialer, opts, err := accountTransport(cfg, reg, acct, mode, id)
		if err != nil {
			return nil, err
		}
		if browserSpec != nil {
			opts = append(opts, hhapi.WithBrowserTLSSpec(browserSpec))
		}
		// T58: the replace flow deletes forked experience generations after
		// a verified apply; [features.autoclone.improve].experience_replace
		// defaults to true (the owner decision — improve applies forked the
		// clone's records into duplicates), an explicit false opts out.
		if ir := cfg.Features.AutoClone.Improve.ExperienceReplace; ir == nil || *ir {
			opts = append(opts, hhapi.WithExperienceReplace(true))
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
