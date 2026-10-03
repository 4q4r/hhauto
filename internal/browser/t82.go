// t82.go carries the identity-parity primitives shared by the capture
// session and the browser lanes: the capture UA gate and the lane-side
// spec-UA pin. The captured spec is the single source of truth — its UA
// is what every lane must present, whatever the raw browser would say.

package browser

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"hhauto/internal/tlsfp"
)

// validateSpecUserAgent gates the capture session UA before it is
// stamped into the spec (T82 owner rule: the spec carries the stealth
// profile's own UA or nothing). A headless-marker UA means the session
// was not the identity we replay — the capture must fail before anything
// is saved.
func validateSpecUserAgent(ua string) error {
	if strings.TrimSpace(ua) == "" {
		return errors.New("browser: tls probe: browser reported an empty User-Agent")
	}
	if strings.Contains(ua, "HeadlessChrome") {
		return fmt.Errorf("browser: tls probe: session UA carries the HeadlessChrome marker: %q", ua)
	}
	return nil
}

// SpecUserAgentFlags returns the launch override pinning a browser lane's
// identity to the captured spec UA (T82: one source of truth for every
// lane, durable across browser updates). The switch sets both the
// User-Agent request header and navigator.userAgent from the first byte.
// An empty spec UA leaves the lane on the browser's own identity.
func SpecUserAgentFlags(specUA string) []StealthFlag {
	if strings.TrimSpace(specUA) == "" {
		return nil
	}
	return []StealthFlag{{Name: "user-agent", Value: specUA}}
}

// LaneUserAgent reads the captured spec UA for a browser lane (T82). A
// missing or broken spec returns "" — the lane then runs on the
// browser's own identity; the engine owns the loud recapture on its
// startup path, so a lane hitting this is already preceded by a failure
// or a fresh install.
func LaneUserAgent(dataDir string, log *slog.Logger) string {
	spec, err := tlsfp.Load(tlsfp.DefaultPath(dataDir))
	if err != nil {
		log.Warn("browser: спека TLS недоступна — дорога использует собственный UA браузера", "err", err)
		return ""
	}
	return spec.UserAgent
}
