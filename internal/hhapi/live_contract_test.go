package hhapi

// Live-contract tests: the parsers are run over anonymized fixtures derived
// from real hh.ru captures (2026-09-24, authorized owner session). Shapes and
// assertions below mirror the captured payloads; see testdata/live/README.md.

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLive(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "live", name))
	if err != nil {
		t.Fatalf("read live fixture %s: %v", name, err)
	}
	return b
}

// liveNow anchors window math to the capture date so the assertions on the
// fixtures are deterministic regardless of when the suite runs.
var liveNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestLiveCounts(t *testing.T) {
	n, err := parseCounts(readLive(t, "counts.json"))
	if err != nil {
		t.Fatalf("parseCounts: %v", err)
	}
	if n != 1630 {
		t.Fatalf("counts = %d, want 1630", n)
	}
}

func TestLiveCountsRejectsMissing(t *testing.T) {
	if _, err := parseCounts([]byte(`{"nope":1}`)); err == nil {
		t.Fatal("expected error for payload without counts")
	}
}

func TestLiveResumes(t *testing.T) {
	resumes, err := parseResumes(readLive(t, "resumes.json"))
	if err != nil {
		t.Fatalf("parseResumes: %v", err)
	}
	if len(resumes) != 3 {
		t.Fatalf("resumes = %d, want 3", len(resumes))
	}
	for _, r := range resumes {
		if len(r.HHID) < 32 {
			t.Fatalf("resume hash %q too short", r.HHID)
		}
		if r.Status != "modified" {
			t.Fatalf("status = %q, want \"modified\" (live _attributes form)", r.Status)
		}
		if !r.Searchable {
			t.Fatalf("isSearchable not read from _attributes: %+v", r)
		}
		if r.Percent != 0 {
			t.Fatalf("percent = %d", r.Percent)
		}
		if r.UpdatedAt.IsZero() || r.UpdatedAt.Year() != 2026 {
			t.Fatalf("updated not parsed from _attributes ms-epoch: %v", r.UpdatedAt)
		}
		if len(r.Raw) == 0 {
			t.Fatal("raw payload dropped")
		}
	}
}

func TestLiveApplyPopup(t *testing.T) {
	meta, err := parseApplyPopup(readLive(t, "popup.json"), 10000046)
	if err != nil {
		t.Fatalf("parseApplyPopup: %v", err)
	}
	if meta.HasTests {
		t.Fatal("hasTests should come from responseStatus.test")
	}
	if meta.LetterRequired {
		t.Fatal("letterRequired should be shortVacancy.@responseLetterRequired (false)")
	}
	if !meta.AcceptAutoResponse {
		t.Fatal("acceptAutoResponse should come from shortVacancy.autoResponse")
	}
	if meta.ChatWritePossibility != "ENABLED_AFTER_INVITATION" {
		t.Fatalf("chatWritePossibility = %q", meta.ChatWritePossibility)
	}
	if len(meta.ResumeInconsistencies) != 0 {
		t.Fatalf("inconsistencies = %+v", meta.ResumeInconsistencies)
	}
	if meta.AlreadyApplied || meta.ResponseImpossible {
		t.Fatalf("alreadyApplied/responseImpossible = %v/%v", meta.AlreadyApplied, meta.ResponseImpossible)
	}
}

func TestLiveApplyPopupLetterRequiredFromShortVacancy(t *testing.T) {
	// The only live occurrence of the letter gate is the @-prefixed
	// shortVacancy key; the parser must read exactly that.
	body := []byte(`{"responseStatus":{"test":{"hasTests":true},"shortVacancy":{"@responseLetterRequired":true,"vacancyId":42,"chatWritePossibility":"ALLOWED"},"alreadyApplied":true}}`)
	meta, err := parseApplyPopup(body, 42)
	if err != nil {
		t.Fatalf("parseApplyPopup: %v", err)
	}
	if !meta.LetterRequired || !meta.HasTests || !meta.AlreadyApplied {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestLiveEmployerRating(t *testing.T) {
	r, err := parseEmployerRating(readLive(t, "rating.json"), 10000007, liveNow)
	if err != nil {
		t.Fatalf("parseEmployerRating: %v", err)
	}
	if r.EmployerID != 10000007 {
		t.Fatalf("employer id = %d", r.EmployerID)
	}
	// Live widget: totalRating is the STRING "4.2".
	if r.ScoreAll != 4.2 {
		t.Fatalf("score_all = %v, want 4.2 (string totalRating)", r.ScoreAll)
	}
	if r.ReviewsCount != 791 || r.RecommendPct != 82 {
		t.Fatalf("counts = %d/%d", r.ReviewsCount, r.RecommendPct)
	}
	if r.ReviewsCountRecent != 21 {
		t.Fatalf("reviews_count_recent = %d, want 21 (last3MonthsReviewsInfo.reviewsCount)", r.ReviewsCountRecent)
	}
	// Window mean over the deduped review lists: 10 entries from
	// reviewsWithoutFeedback (one overlaps reviews[]) + the most recent
	// negative review, all inside 90 days of liveNow => 44.8/11.
	want := 44.8 / 11
	if math.Abs(r.ScoreRecent-want) > 1e-9 {
		t.Fatalf("score_recent = %v, want %v (mean over dated window entries)", r.ScoreRecent, want)
	}
}

func TestEmployerRatingLegacyLast3MonthsWins(t *testing.T) {
	body := []byte(`{"employerReviews":{"totalRating":"4.0","reviewsCount":10,"recommendationsPercent":50,
		"last3MonthsReviewsInfo":{"totalRating":"4.6","reviewsCount":2}}}`)
	r, err := parseEmployerRating(body, 5, liveNow)
	if err != nil {
		t.Fatalf("parseEmployerRating: %v", err)
	}
	if r.ScoreRecent != 4.6 || r.ScoreAll != 4.0 || r.ReviewsCountRecent != 2 {
		t.Fatalf("rating = %+v", r)
	}
}

func TestEmployerRatingWindowFallback(t *testing.T) {
	// Dated entries exist but all fall outside the window: fall back to the
	// all-time score (documented heuristic).
	body := []byte(`{"employerReviews":{"totalRating":"3.9","reviewsCount":30,"recommendationsPercent":40,
		"reviews":[{"id":"1","totalRating":"1.0","creationTime":"2019-01-01T00:00:00Z"}]}}`)
	r, err := parseEmployerRating(body, 6, liveNow)
	if err != nil {
		t.Fatalf("parseEmployerRating: %v", err)
	}
	if r.ScoreRecent != 3.9 {
		t.Fatalf("score_recent = %v, want all-time fallback 3.9", r.ScoreRecent)
	}
}

func TestLiveListChats(t *testing.T) {
	chats, err := parseChatsList(readLive(t, "chats-list.json"))
	if err != nil {
		t.Fatalf("parseChatsList: %v", err)
	}
	if len(chats) != 20 {
		t.Fatalf("chats = %d, want 20", len(chats))
	}
	states := map[string]int{}
	withTopic, withVacancy, withTime := 0, 0, 0
	for _, c := range chats {
		if c.ID == "" {
			t.Fatal("chat id empty: live int id must be stringified")
		}
		if c.LastMessageAt.IsZero() {
			t.Fatal("lastMessageAt not parsed from lastMessage.creationTime")
		}
		withTime++
		if c.TopicID != "" {
			withTopic++
		}
		if c.VacancyID != 0 {
			withVacancy++
		}
		if c.ApplicantState != "" {
			states[c.ApplicantState]++
		}
	}
	if withTime != 20 || withTopic == 0 || withVacancy == 0 {
		t.Fatalf("topic=%d vacancy=%d time=%d", withTopic, withVacancy, withTime)
	}
	if states["DISCARD"] == 0 || states["RESPONSE"] == 0 {
		t.Fatalf("applicant states not surfaced: %v (want DISCARD + RESPONSE)", states)
	}
	// First chat mirrors the capture head: employer identity from the
	// display-info subtitle.
	if !strings.Contains(chats[0].EmployerName, "Company N") {
		t.Fatalf("employer name = %q", chats[0].EmployerName)
	}
	if chats[0].ID != "10000045" {
		t.Fatalf("chat id = %q, want stringifyed int 10000045", chats[0].ID)
	}
}

func TestLiveChatListRejectsGarbage(t *testing.T) {
	if _, err := parseChatsList([]byte("not json")); err == nil {
		t.Fatal("expected error for non-JSON body")
	}
}

func TestIsStructuralRefusal(t *testing.T) {
	for _, s := range []string{"DISCARD", "discard", "refused", "REFUSED", "Refused"} {
		if !IsStructuralRefusal(s) {
			t.Fatalf("IsStructuralRefusal(%q) = false", s)
		}
	}
	for _, s := range []string{"", "RESPONSE", "INVITATION", "discard " + "\x00"} {
		if IsStructuralRefusal(s) {
			t.Fatalf("IsStructuralRefusal(%q) = true", s)
		}
	}
}

func TestChatHistorySkipsWorkflowTransitions(t *testing.T) {
	// Live chats carry numeric workflowTransition ids plus applicantState;
	// such system events must not leak into the text pipeline.
	body := []byte(`{"chat":{"currentParticipantId":"111","messages":{"items":[
		{"id":"m1","type":"SIMPLE","participantId":"222","text":"привет","createDateTime":"2026-09-24T10:00:00+03:00"},
		{"id":"m2","type":"SIMPLE","participantId":"222","text":"","createDateTime":"2026-09-24T10:01:00+03:00",
		 "workflowTransition":{"id":15605425352,"topicId":10000047,"applicantState":"DISCARD","declinedByApplicant":false}},
		{"id":"m3","type":"SYSTEM","participantId":"222","text":"sys"}
	]}}}`)
	msgs, err := parseChatHistory(body, "555001")
	if err != nil {
		t.Fatalf("parseChatHistory: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "привет" || msgs[0].Direction != "in" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestCountsMethod(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/shards/vacancy/counts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("text") != "golang" {
			t.Errorf("query not forwarded: %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"counts":1630}`))
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	n, err := c.Counts(ctx, "text=golang")
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if n != 1630 {
		t.Fatalf("counts = %d", n)
	}
}
