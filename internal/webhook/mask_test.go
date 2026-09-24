package webhook

// Б3c regression: endpoint ids are full URLs and Telegram/Discord URLs
// carry bot credentials. Every log line that prints an endpoint id or a
// transport error must go through MaskEndpoint — nothing secret in logs.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
)

func TestMaskEndpointMasksBotCredentials(t *testing.T) {
	cases := []struct{ in, want, gone string }{
		{
			in:   "https://api.telegram.org/bot123456789:AAF3supersecretTOKEN/sendMessage",
			want: "https://api.telegram.org/bot***/sendMessage",
			gone: "AAF3supersecretTOKEN",
		},
		{
			in:   "https://discord.com/api/webhooks/1234567890/aVerySecretToken",
			want: "https://discord.com/api/webhooks/1234567890/***",
			gone: "aVerySecretToken",
		},
	}
	for _, tc := range cases {
		got := MaskEndpoint(tc.in)
		if got != tc.want {
			t.Fatalf("MaskEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, tc.gone) {
			t.Fatalf("MaskEndpoint(%q) = %q leaks %q", tc.in, got, tc.gone)
		}
	}
}

func TestDeliveryFailureLogMasksEndpoint(t *testing.T) {
	const secret = "AAF3supersecretTOKEN"
	endpointURL := "http://127.0.0.1:1/bot123456789:" + secret + "/sendMessage"

	var buf bytes.Buffer
	n := New(
		[]config.Webhook{{
			URL:    endpointURL,
			Preset: "telegram", ChatID: "42",
		}},
		nil, nil, slog.New(slog.NewTextHandler(&buf, nil)),
	)
	for _, ep := range n.endpoints {
		ep.backoff = func(int) time.Duration { return 0 }
	}
	n.Emit(Event{Type: EvNeedsHuman, Message: "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, secret) {
		t.Fatalf("delivery failure log leaks the bot token:\n%s", logged)
	}
	if !strings.Contains(logged, "bot***") {
		t.Fatalf("delivery failure log must mask the endpoint id, got:\n%s", logged)
	}
}
