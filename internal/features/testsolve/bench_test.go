package testsolve

// Benchmarks for the two pure parsers of the testsolve feature: external
// link detection (the shadow-ban guard over assessment texts) and the HTML
// form parser (draft_only answer extraction). Both run per assessment
// without network.

import (
	"os"
	"path/filepath"
	"testing"
)

func benchFixture(b *testing.B, name string) string {
	b.Helper()
	doc, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		b.Fatalf("read fixture %s: %v", name, err)
	}
	return string(doc)
}

// benchLinkText is a typical assessment prompt with a couple of external
// links mixed into prose (~1.5KB) so the regex scan does real work.
var benchLinkText = buildLinkBenchText()

func buildLinkBenchText() string {
	para := "Внимательно прочитайте условие задачи и выберите правильный вариант ответа. " +
		"Если возникнут вопросы, напишите в чат поддержки. Решение засчитывается один раз. "
	text := "Инструкция к тесту. Перейдите по ссылке https://forms.example.com/assessment/42 и " +
		"заполните поля. Телеграм-канал курса: t.me/example_course. Вопросы: @support_bot. " +
		"Дубликат инструкции: www.example.com/help. "
	for len(text) < 1536 {
		text += para
	}
	return text
}

func BenchmarkDetectExternalLinks(b *testing.B) {
	b.SetBytes(int64(len(benchLinkText)))
	b.ReportAllocs()
	var links []string
	for b.Loop() {
		links = DetectExternalLinks(benchLinkText)
	}
	if len(links) == 0 {
		b.Fatal("no links detected in fixture text")
	}
}

func BenchmarkParseFormHTML(b *testing.B) {
	doc := benchFixture(b, "form_ok.html")
	if _, ok := ParseFormHTML(doc); !ok {
		b.Fatal("form fixture does not parse")
	}
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := ParseFormHTML(doc); !ok {
			b.Fatal("parse failed mid-benchmark")
		}
	}
}
