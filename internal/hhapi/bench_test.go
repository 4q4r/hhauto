package hhapi

// Benchmarks over the live-contract fixtures (testdata/live): the same
// anonymized hh.ru captures the contract tests assert against, so ns/op and
// B/s track real payload shapes. b.SetBytes(file size) turns the results
// into parser throughput (MB/s); allocs/op surfaces per-parse garbage.
//
// Heavy fixtures (rating 126K, chats 192K) are meant to run with a fixed
// iteration count: go test -bench . -benchtime=10x ./internal/hhapi/

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// benchNow anchors window math to the capture date (same anchor as the
// live-contract tests), so rating-window scanning is deterministic.
var benchNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func benchLive(b *testing.B, name string) []byte {
	b.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "live", name))
	if err != nil {
		b.Fatalf("read live fixture %s: %v", name, err)
	}
	return body
}

func BenchmarkParseResumes(b *testing.B) {
	body := benchLive(b, "resumes.json")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseResumes(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParsePopup(b *testing.B) {
	body := benchLive(b, "popup.json")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseApplyPopup(body, 10000046); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseRating(b *testing.B) {
	body := benchLive(b, "rating.json")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseEmployerRating(body, 10000007, benchNow); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseChats(b *testing.B) {
	body := benchLive(b, "chats-list.json")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseChatsList(body); err != nil {
			b.Fatal(err)
		}
	}
}
