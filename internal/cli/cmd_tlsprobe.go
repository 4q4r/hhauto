package cli

// T37 v3: tls-probe captures the cloakbrowser chromium ClientHello and
// caches the 1:1 TLS replay spec under data_dir/browser/tls_spec.json.
// The hhapi tls-client transport replays that hello on every hh
// connection. T50: the engine runs the same capture automatically at
// startup, so the command is a manual retrigger, not a prerequisite;
// there is no preset-profile fallback anymore. It recaptures
// automatically when the resolved browser version or the capture host
// changed; --force skips the checks.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"hhauto/internal/browser"
	"hhauto/internal/logging"
	"hhauto/internal/tlsfp"
)

func newTLSProbeCmd() *cobra.Command {
	var (
		force bool
		host  string
	)
	cmd := &cobra.Command{
		Use:   "tls-probe",
		Short: "Capture the cloakbrowser ClientHello and refresh the 1:1 TLS replay spec",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := LoadConfigOnly(flags.configPath)
			if err != nil {
				return exitErr("config: %v", err)
			}
			log, closeLog := logging.New(cfg.Log)
			defer func() { _ = closeLog() }()

			probeHost := host
			if probeHost == "" {
				probeHost = browser.ProbeHost(cfg.API.Endpoints.Site)
			}
			path := tlsfp.DefaultPath(cfg.General.DataDir)

			if !force && specUpToDate(ctx, browser.NewFromConfig(cfg.Browser, log), path, probeHost, log) {
				cur, lerr := tlsfp.Load(path)
				if lerr != nil {
					return exitErr("tls-probe: cached spec vanished: %v", lerr)
				}
				outFor(cmd).println(fmt.Sprintf(
					"TLS-спека актуальна: браузер %s, хост %s, ja3_md5 %s (%s)",
					cur.BrowserVer, probeHost, cur.JA3MD5, path))
				return nil
			}

			spec, err := browser.CaptureTLSSpec(ctx, cfg, probeHost, log)
			if err != nil {
				return exitErr("tls-probe: %v", err)
			}
			outFor(cmd).println(fmt.Sprintf(
				"ClientHello захвачен и проверен (replay-verified).\n"+
					"  браузер: %s\n  хост: %s\n  UA: %s\n  расширений: %d, ciphers: %d\n  ja3 (normalized): %s\n  ja3_md5: %s\n  кэш: %s",
				spec.BrowserVer, spec.Host, spec.UserAgent, len(spec.Extensions), len(spec.CipherSuites),
				spec.JA3, spec.JA3MD5, path))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "recapture even when the cached spec matches the current browser version")
	cmd.Flags().StringVar(&host, "host", "", "capture host (default: the configured hh site host)")
	return cmd
}

// specUpToDate reports whether the cached spec matches the currently
// resolved browser version and the requested capture host.
func specUpToDate(ctx context.Context, provider *browser.CloakProvider, path, host string, log *slog.Logger) bool {
	cur, err := tlsfp.Load(path)
	if err != nil {
		return false
	}
	chromePath, cleanup, err := provider.Chrome(ctx)
	if err != nil {
		return false
	}
	defer cleanup()
	ver := browser.ChromeVersion(ctx, chromePath, log)
	if cur.BrowserVer == ver && cur.Host == host {
		return true
	}
	log.Info("browser: tls-probe: recapture needed",
		"spec_browser_version", cur.BrowserVer, "current_browser_version", ver,
		"spec_host", cur.Host, "host", host)
	return false
}
