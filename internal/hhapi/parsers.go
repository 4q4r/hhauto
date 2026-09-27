package hhapi

// Live-shape parsers for the hh.ru web payloads. Each parser is a pure
// function over a response body so the fixtures in testdata/live exercise
// exactly the code that runs against the site (see testdata/live/README.md
// for the captured shapes). Legacy/donor-era flat shapes remain supported as
// fallbacks for surfaces not re-verified live; the live shape always wins.

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ratingRecentWindow is the recent-window span used when the widget does not
// publish an explicit recent rating. The live widget exposes last3Months
// info, so 90 days is the documented window (parseEmployerRating).
const ratingRecentWindow = 90 * 24 * time.Hour

// scalarString renders a JSON scalar (string or integral number) as a string.
// Live chatik ids are numbers while the Chat contract carries strings.
func scalarString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if f := scalarNum(raw); !math.IsInf(f, 0) && f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}

// scalarNum reads a JSON scalar as a number (numeric strings tolerated).
func scalarNum(raw json.RawMessage) float64 {
	if raw == nil {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, e := strconv.ParseFloat(s, 64); e == nil {
			return v
		}
	}
	return 0
}

// epochMS parses hh millisecond-epoch fields (live `_attributes.updated`)
// falling back to the string time layouts.
func epochMS(raw json.RawMessage) time.Time {
	if raw == nil {
		return time.Time{}
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f > 0 {
		sec, frac := math.Modf(f)
		return time.UnixMilli(int64(sec)).Add(time.Duration(frac * float64(time.Millisecond)))
	}
	return parseHHTime(scalarString(raw))
}

// firstStr returns the first element of a raw array that renders as a
// non-empty string (live chat resources.{VACANCY,RESUME,NEGOTIATION_TOPIC}).
func firstStr(vals []json.RawMessage) string {
	for _, raw := range vals {
		if s := scalarString(raw); s != "" {
			return s
		}
	}
	return ""
}

// --- counts ---

func parseCounts(body []byte) (int, error) {
	var payload struct {
		Counts json.RawMessage `json:"counts"`
		Count  json.RawMessage `json:"count"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("hhapi: counts: %w: %s", err, snippet(body))
	}
	if payload.Counts != nil {
		return int(scalarNum(payload.Counts)), nil
	}
	if payload.Count != nil {
		return int(scalarNum(payload.Count)), nil
	}
	return 0, fmt.Errorf("hhapi: counts: %w (no counts key)", ErrNotFound)
}

// --- resumes ---

func parseResumes(body []byte) ([]Resume, error) {
	var payload struct {
		Resumes []json.RawMessage `json:"resumes"`
		Items   []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("hhapi: list_resumes: %w: %s", err, snippet(body))
	}
	raws := payload.Resumes
	if raws == nil {
		raws = payload.Items
	}
	resumes := make([]Resume, 0, len(raws))
	for _, raw := range raws {
		m := obj(raw)
		attrs := obj(m["_attributes"])
		// Live mine-shard: every field lives inside _attributes (the entry
		// itself only adds _conditions/professionalRole). status arrives as
		// a plain string; older surfaces used objects.
		status := firstNonEmpty(
			str(attrs, "status"),
			str(obj(attrs["status"]), "id", "name"),
			str(m, "status"),
			str(obj(m["status"]), "id", "name"),
		)
		updated := epochMS(attrs["updated"])
		if updated.IsZero() {
			updated = epochMS(m["updated"])
		}
		created := epochMS(attrs["created"])
		if created.IsZero() {
			created = epochMS(m["created"])
		}
		nextTouch := epochMS(attrs["nextTouchAt"])
		if nextTouch.IsZero() {
			nextTouch = epochMS(m["nextTouchAt"])
		}
		if updated.IsZero() {
			updated = timeFrom(m, "updated_at", "updatedAt", "modify_time")
		}
		resumes = append(resumes, Resume{
			HHID:       firstNonEmpty(str(attrs, "hash"), scalarString(attrs["id"]), str(m, "hash", "resumeHash", "resume_hash", "id")),
			Title:      firstNonEmpty(str(m, "title"), str(attrs, "title")),
			Status:     status,
			Percent:    int(num(attrs, "percent")),
			Searchable: boolv(attrs, "isSearchable") || boolv(m, "isSearchable", "is_searchable"),
			UpdatedAt:  updated,
			Raw:        raw,

			ParentResumeID: str(attrs, "parentResumeId"),
			CreatedAt:      created,
			NextTouchAt:    nextTouch,
		})
	}
	return resumes, nil
}

// --- apply popup ---

func parseApplyPopup(body []byte, vacancyID int64) (*ApplyMeta, error) {
	var env struct {
		ResponseStatus struct {
			Test struct {
				HasTests bool `json:"hasTests"`
			} `json:"test"`
			ShortVacancy struct {
				LetterRequired       json.RawMessage `json:"@responseLetterRequired"`
				ChatWritePossibility string          `json:"chatWritePossibility"`
				AutoResponse         struct {
					AcceptAutoResponse json.RawMessage `json:"acceptAutoResponse"`
				} `json:"autoResponse"`
			} `json:"shortVacancy"`
			ResumeInconsistencies json.RawMessage `json:"resumeInconsistencies"`
			NegotiationsLimit     json.RawMessage `json:"negotiationsLimitExceeded"`
			AlreadyApplied        bool            `json:"alreadyApplied"`
			ResponseImpossible    bool            `json:"responseImpossible"`
		} `json:"responseStatus"`
		// Legacy flat shapes (donor-era surfaces and test doubles).
		Test struct {
			HasTests bool `json:"hasTests"`
		} `json:"test"`
		LetterRequired        json.RawMessage `json:"letterRequired"`
		AcceptAutoResponse    json.RawMessage `json:"acceptAutoResponse"`
		ChatWritePossibility  string          `json:"chatWritePossibility"`
		ResumeInconsistencies json.RawMessage `json:"resumeInconsistencies"`
		NegotiationsLimit     bool            `json:"negotiationsLimitExceeded"`
		AlreadyApplied        bool            `json:"alreadyApplied"`
		ResponseImpossible    bool            `json:"responseImpossible"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("hhapi: apply_popup %d: %w: %s", vacancyID, err, snippet(body))
	}
	rs := env.ResponseStatus
	if jsonBool(rs.NegotiationsLimit) || env.NegotiationsLimit {
		return nil, &StatusError{Kind: ErrRateLimited}
	}
	inconsistencies := rs.ResumeInconsistencies
	if inconsistencies == nil {
		inconsistencies = env.ResumeInconsistencies
	}
	return &ApplyMeta{
		HasTests:              rs.Test.HasTests || env.Test.HasTests,
		LetterRequired:        jsonBool(rs.ShortVacancy.LetterRequired) || jsonBool(env.LetterRequired),
		AcceptAutoResponse:    jsonBool(rs.ShortVacancy.AutoResponse.AcceptAutoResponse) || jsonBool(env.AcceptAutoResponse),
		ChatWritePossibility:  firstNonEmpty(rs.ShortVacancy.ChatWritePossibility, env.ChatWritePossibility),
		ResumeInconsistencies: flattenInconsistencies(inconsistencies),
		AlreadyApplied:        rs.AlreadyApplied || env.AlreadyApplied,
		ResponseImpossible:    rs.ResponseImpossible || env.ResponseImpossible,
	}, nil
}

// flattenInconsistencies converts the nested resumeInconsistencies block
// into short human-readable strings ("TYPE: required X, actual Y"). The live
// payload carries an object/dict (empty means no inconsistencies).
func flattenInconsistencies(raw json.RawMessage) []string {
	root := obj(raw)
	out := []string{}
	for _, resBlock := range arr(root["resume"]) {
		res := obj(resBlock)
		inc := obj(res["inconsistencies"])
		items := arr(inc["inconsistency"])
		if items == nil {
			items = arr(res["inconsistency"])
		}
		for _, item := range items {
			m := obj(item)
			typ := str(m, "type")
			req := str(m, "required")
			act := str(m, "actual")
			s := typ
			if req != "" || act != "" {
				s += fmt.Sprintf(": required %s, actual %s", req, act)
			}
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// --- employer rating ---

func parseEmployerRating(body []byte, employerID int64, now time.Time) (*EmployerRating, error) {
	var payload struct {
		EmployerReviews json.RawMessage `json:"employerReviews"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("hhapi: employer_rating %d: %w: %s", employerID, err, snippet(body))
	}
	rev := obj(payload.EmployerReviews)
	if rev == nil || str(rev, "totalRating") == "" {
		return nil, fmt.Errorf("hhapi: employer_rating %d: %w (no employerReviews.totalRating)", employerID, ErrNotFound)
	}
	rating := &EmployerRating{
		EmployerID:   employerID,
		ScoreAll:     num(rev, "totalRating"), // live: STRING ("4.2")
		ReviewsCount: int(num(rev, "reviewsCount")),
		RecommendPct: int(num(rev, "recommendationsPercent")),
		FetchedAt:    now,
	}
	recent := obj(rev["last3MonthsReviewsInfo"])
	if recent != nil {
		rating.ReviewsCountRecent = int(num(recent, "reviewsCount"))
		// Surfaces that publish an explicit recent rating win outright.
		if s := str(recent, "totalRating"); s != "" {
			rating.ScoreRecent = num(recent, "totalRating")
			return rating, nil
		}
	}
	// Live small_widget carries only counts in last3MonthsReviewsInfo — no
	// recent rating. Heuristic (documented, M0-verified): the dated review
	// lists (reviews / reviewsWithoutFeedback / negativeReviews, deduped by
	// id) expose per-review totalRating + creationTime, so ScoreRecent is
	// the mean over entries dated inside the recent window; with no
	// in-window entry it falls back to the all-time score. The widget
	// truncates the lists, so the mean estimates the visible sample only.
	if mean, ok := reviewWindowMean(rev, now); ok {
		rating.ScoreRecent = mean
	} else {
		rating.ScoreRecent = rating.ScoreAll
	}
	return rating, nil
}

func reviewWindowMean(rev map[string]json.RawMessage, now time.Time) (float64, bool) {
	seen := make(map[string]bool)
	cutoff := now.Add(-ratingRecentWindow)
	var sum float64
	var n int
	for _, key := range []string{"reviews", "reviewsWithoutFeedback", "negativeReviews"} {
		for _, raw := range arr(rev[key]) {
			m := obj(raw)
			if id := scalarString(m["id"]); id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			rating := scalarNum(m["totalRating"])
			when := timeFrom(m, "creationTime")
			if rating <= 0 || when.IsZero() {
				continue
			}
			if when.After(cutoff) && !when.After(now) {
				sum += rating
				n++
			}
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// --- chats ---

func parseChatsList(body []byte) ([]Chat, error) {
	var payload struct {
		Chats struct {
			Items []json.RawMessage `json:"items"`
		} `json:"chats"`
		DisplayInfo map[string]json.RawMessage `json:"chatsDisplayInfo"`
		Resources   struct {
			NegotiationTopics map[string]json.RawMessage `json:"negotiation_topics"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("hhapi: chats: %w: %s", err, snippet(body))
	}
	out := make([]Chat, 0, len(payload.Chats.Items))
	for _, raw := range payload.Chats.Items {
		m := obj(raw)
		// Live item ids are ints; the contract type is the string form.
		chatID := firstNonEmpty(scalarString(m["id"]), str(m, "chatId"))
		info := obj(payload.DisplayInfo[chatID])
		res := obj(m["resources"])
		last := obj(m["lastMessage"])
		wf := obj(last["workflowTransition"])
		topic := obj(m["topic"])

		topicRef := firstStr(arr(res["NEGOTIATION_TOPIC"]))
		if topicRef == "" {
			topicRef = firstNonEmpty(scalarString(wf["topicId"]), scalarString(m["topicId"]), str(topic, "id"), str(obj(m["negotiation"]), "id"))
		}
		state := ""
		if topicState := obj(payload.Resources.NegotiationTopics[topicRef]); topicState != nil {
			state = firstNonEmpty(str(topicState, "currentApplicantState"), str(topicState, "initialApplicantState"))
		}
		if state == "" {
			state = str(wf, "applicantState")
		}

		var vacancyID int64
		if vacRef := firstStr(arr(res["VACANCY"])); vacRef != "" {
			vacancyID = int64(scalarNum(json.RawMessage(vacRef)))
		} else {
			vacancyID = int64(num(m, "vacancyId"))
		}

		out = append(out, Chat{
			ID:             chatID,
			TopicID:        topicRef,
			VacancyID:      vacancyID,
			EmployerName:   firstNonEmpty(str(info, "subtitle"), str(info, "title", "employerName", "name"), str(m, "employerName")),
			Unread:         int(num(m, "unreadCount", "unread")),
			LastMessageAt:  timeFrom(last, "creationTime", "createDateTime", "createdAt", "sentAt"),
			ApplicantState: state,
		})
	}
	return out, nil
}

// --- chat history ---

func parseChatHistory(body []byte, chatID string) ([]ChatMessage, error) {
	var payload struct {
		Chat struct {
			CurrentParticipantID string `json:"currentParticipantId"`
			Messages             struct {
				Items []json.RawMessage `json:"items"`
			} `json:"messages"`
		} `json:"chat"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("hhapi: chat_data %s: %w: %s", chatID, err, snippet(body))
	}
	self := payload.Chat.CurrentParticipantID
	out := make([]ChatMessage, 0, len(payload.Chat.Messages.Items))
	for _, raw := range payload.Chat.Messages.Items {
		m := obj(raw)
		typ := str(m, "type")
		if typ != "" && typ != "SIMPLE" {
			continue
		}
		// Workflow transitions are system events (rejection, offer, ...):
		// typed by applicantState on the live payload, or a non-empty id
		// (string on older surfaces, numeric int live).
		if wf := obj(m["workflowTransition"]); wf != nil {
			if str(wf, "applicantState") != "" || str(wf, "id") != "" || scalarNum(wf["id"]) != 0 {
				continue
			}
		}
		sender := str(m, "participantId", "authorParticipantId")
		direction := "in"
		if sender != "" && sender == self {
			direction = "out"
		}
		display := obj(m["participantDisplay"])
		out = append(out, ChatMessage{
			ID:        firstNonEmpty(scalarString(m["id"]), str(m, "messageId")),
			Author:    str(display, "name", "fullName"),
			Direction: direction,
			Text:      str(m, "text"),
			SentAt:    timeFrom(m, "creationTime", "createDateTime", "createdAt", "sentAt"),
		})
	}
	return out, nil
}
