package webhook

import "regexp"

// Endpoint ids ARE full URLs, so every log line that would print an
// endpoint id (or an error embedding one) goes through MaskEndpoint first:
// Telegram bot tokens (…/bot<id>:<secret>/…) and Discord webhook tokens
// (…/webhooks/<id>/<token>) must never reach the logs in clear text.
var (
	telegramBotTokenRe = regexp.MustCompile(`bot[0-9]+:[A-Za-z0-9_-]+`)
	discordWebhookRe   = regexp.MustCompile(`(/webhooks/[0-9]+/)[^/?]+`)
)

// MaskEndpoint masks bot credentials embedded in an endpoint URL/identifier
// for logging: "https://api.telegram.org/bot123:ABC/sendMessage" logs as
// "https://api.telegram.org/bot***/sendMessage".
func MaskEndpoint(s string) string {
	s = telegramBotTokenRe.ReplaceAllLiteralString(s, "bot***")
	return discordWebhookRe.ReplaceAllString(s, "$1***")
}
