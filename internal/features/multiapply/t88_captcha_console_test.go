package multiapply

// T88: the unified human captcha console surface — the first challenge on
// apply renders the yellow captcha line (never the raw payload), the
// breaker renders one Russian banner per trip, and the exhausted skips
// carry the Russian label — over the fake pipeline, real pass.

import (
	"strings"
	"testing"

	"hhauto/internal/config"
)

// The first captcha challenge (no solver chain): the yellow captcha line
// with title and employer — not the red error, not the raw challenge text.
func TestConsoleCaptchaFirstHit(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "none" })
	h.noChain = true
	h.searchItems = []string{vacItem(777060, "Специалист по автоматизации", 9001, "X5 Tech", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777060] = popupWith(false, false, "ENABLED")
	h.applyErr = func(int, int64, string) error {
		return &respErr{redirect: "/captcha/image?backurl=x"}
	}

	h.run(t)

	out := buf.String()
	if !strings.Contains(out, "⚑ Капча: «Специалист по автоматизации» — X5 Tech · решателя не настроено, вакансия пропущена до следующего цикла") {
		t.Fatalf("captcha line missing: %q", out)
	}
	if strings.Contains(out, "captcha challenge") || strings.Contains(out, "hhcaptcha") || strings.Contains(out, "captchaState") {
		t.Fatalf("raw challenge payload leaked onto the console: %q", out)
	}
	if strings.Contains(out, "✗ Ошибка отклика") {
		t.Fatalf("captcha must not render the red error line: %q", out)
	}
	// The failure still counts (digest and metrics unchanged).
	if !strings.Contains(out, "ошибок 1") {
		t.Fatalf("digest must still count the challenge: %q", out)
	}
}

// The breaker banner renders exactly once per trip (one event, one banner);
// the vacancy that hit the challenge gets the captcha line, the rest of the
// pass skips with the Russian exhausted label.
func TestConsoleCaptchaBreakerOnce(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "interesting,aggregate" })
	h.noChain = true // the owner's setup: no solver chain configured
	h.searchItems = []string{
		vacItem(777061, "Go Dev A", 9001, "Corp LLC", ""),
		vacItem(777062, "Go Dev B", 9002, "Other LLC", ""),
	}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.ratings[9002] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(_ int, vacancy int64, _ string) error {
		// Both vacancies discover their own challenge with a fresh POST;
		// the breaker's job is to skip the Solve call for v2, not the POST.
		_ = vacancy
		return &respErr{redirect: "/captcha/image?backurl=x"}
	}

	h.run(t)

	out := buf.String()
	if got := strings.Count(out, "⚑ Капча-брейкер: hh начал капчить, решателя нет — остальные вакансии прохода пропущены до следующего цикла"); got != 1 {
		t.Fatalf("breaker banner = %d, want 1: %q", got, out)
	}
	if !strings.Contains(out, "⚑ Капча: «Go Dev A» — Corp LLC") {
		t.Fatalf("captcha line for the challenge vacancy missing: %q", out)
	}
	if !strings.Contains(out, "· Пропущен: «Go Dev B» — Other LLC: капча (лимит прохода)") {
		t.Fatalf("russian exhausted skip label missing: %q", out)
	}
	if strings.Contains(out, "captcha challenge") || strings.Contains(out, "captcha_exhausted") {
		t.Fatalf("machine captcha text leaked onto the console: %q", out)
	}
}
