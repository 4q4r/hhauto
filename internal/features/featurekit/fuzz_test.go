package featurekit

// FuzzNoLinksRule: the no-links rule guards every outgoing text (cover
// letters, chat replies), so hostile input must never panic it and the
// ErrLinkFound contract must stay deterministic. Corpus seeds live in
// testdata/fuzz/FuzzNoLinksRule/.
//
// 30-second run from the T19 pass: 0 failures (see .sdd/t19-report.md).

import "testing"

func FuzzNoLinksRule(f *testing.F) {
	f.Add("")
	f.Add("Здравствуйте! Подхожу под ваш стек: Go, PostgreSQL.")
	f.Add("Подробнее: https://example.com/vacancy")
	f.Add("сайт http://example.org и www.example.net")
	f.Add("канал t.me/jobs_bot, дубль telegram.me/jobs")
	f.Add("Пишите: @username или mail@example.com")
	f.Add("тг: @бот\nskype: live:.cid.abc")
	f.Fuzz(func(t *testing.T, text string) {
		first := NoLinksRule(text)
		second := NoLinksRule(text)
		// Deterministic: same input, same verdict.
		if (first == nil) != (second == nil) {
			t.Fatalf("non-deterministic verdict for %q: %v vs %v", text, first, second)
		}
		if first == nil {
			return
		}
		// Error contract: wraps ErrLinkFound (errors.Is-able).
		if first.Error() == "" {
			t.Fatalf("empty error text for %q", text)
		}
	})
}
