package cli

// Б3b regression: the doctor's captcha-balance probe embeds the provider
// key in the 2captcha query URL; transport errors embed that URL. The error
// returned to stdout must carry key=REDACTED, never the raw key.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hhauto/internal/config"
)

func TestCaptchaBalanceErrorMasksKey(t *testing.T) {
	const key = "SUPERSECRETKEY99"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL
	srv.Close() // force a transport error: the connection is refused now

	for _, protocol := range []string{"2captcha", "anticaptcha"} {
		_, err := captchaBalance(context.Background(), config.CaptchaProvider{
			Protocol: protocol,
			BaseURL:  url,
			Key:      key,
		})
		if err == nil {
			t.Fatalf("%s: expected a transport error", protocol)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("%s: error leaks the raw key: %s", protocol, err)
		}
		if !strings.Contains(err.Error(), "key=REDACTED") {
			t.Fatalf("%s: error must carry key=REDACTED, got: %s", protocol, err)
		}
	}
}
