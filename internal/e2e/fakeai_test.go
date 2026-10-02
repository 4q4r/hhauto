package e2e

// Fake OpenAI Chat Completions provider. The REAL ai.Runner (prompt
// registry, guard, validator, ledger — cli wiring) talks to this server
// through [ai.providers] config; nothing in the ai package is stubbed. The
// fake is deterministic: the task comes from response_format.json_schema
// .name and the verdict is fixed per task, with routing decisions derived
// from the untrusted input block (message text markers).
//
// Every verdict is schema-valid against internal/ai/schemas (additional-
// Properties-faithful: exactly the schema keys, needs_human_reason null).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const e2eModel = "e2e-fake-model"

type fakeAI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string         // task names, in call order
	vacs  []map[string]any // vacancy blocks seen by vacancy_scoring (T81)
}

func newFakeAI(t *testing.T) *fakeAI {
	t.Helper()
	f := &fakeAI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/completions", f.serveChat)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAI) URL() string { return f.srv.URL }

// scoringVacancyInputs returns the vacancy blocks the scorer received, in
// call order (T81: the fake must be able to prove what the model saw).
func (f *fakeAI) scoringVacancyInputs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.vacs))
	copy(out, f.vacs)
	return out
}

func (f *fakeAI) callCount(task string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, name := range f.calls {
		if name == task {
			n++
		}
	}
	return n
}

type aiWireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	ResponseFormat *struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

func (f *fakeAI) serveChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var wire aiWireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	task := ""
	if wire.ResponseFormat != nil {
		task = wire.ResponseFormat.JSONSchema.Name
	}
	user := ""
	for i := len(wire.Messages) - 1; i >= 0; i-- {
		if wire.Messages[i].Role == "user" {
			user = wire.Messages[i].Content
			break
		}
	}
	verdict := f.verdictFor(task, extractInputJSON(user))
	inputMap := extractInputJSON(user)

	f.mu.Lock()
	f.calls = append(f.calls, task)
	if task == "vacancy_scoring" {
		if vac, _ := inputMap["vacancy"].(map[string]any); vac != nil {
			f.vacs = append(f.vacs, vac)
		}
	}
	f.mu.Unlock()

	content, err := json.Marshal(verdict)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.serveJSON(w, map[string]any{
		"model": wire.Model,
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": string(content)},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 42, "completion_tokens": 21},
	})
}

func (f *fakeAI) serveJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// common returns the Common-block keys shared by every task schema.
func e2eCommon(confidence float64) map[string]any {
	return map[string]any{
		"confidence":          confidence,
		"needs_human":         false,
		"needs_human_reason":  nil,
		"injection_suspected": false,
	}
}

// verdictFor builds the deterministic verdict for one task. Classification
// routes on the message text (the same markers the fixtures use); test
// answers echo the question list the runner sent.
func (f *fakeAI) verdictFor(task string, input map[string]any) map[string]any {
	switch task {
	case "cover_letter":
		v := e2eCommon(0.9)
		v["letter"] = e2eLetter
		v["mode"] = "ai_company"
		v["used_facts"] = []any{}
		v["missing_data"] = []any{}
		return v
	case "message_classification":
		category := "other"
		if text := messageTextOf(input); strings.Contains(text, "Тестовое задание") {
			category = "test_assignment"
		} else if strings.Contains(text, "актуальна") {
			category = "question"
		}
		v := e2eCommon(0.95)
		v["category"] = category
		v["evidence_quote"] = nil
		return v
	case "chat_reply":
		v := e2eCommon(0.95)
		v["reply"] = e2eReply
		v["intent"] = "ack"
		v["used_facts"] = []any{}
		return v
	case "test_answer":
		answers := []any{}
		if qs, ok := input["questions"].([]any); ok {
			for _, q := range qs {
				qm, _ := q.(map[string]any)
				text, _ := qm["text"].(string)
				answers = append(answers, map[string]any{
					"question_id":      text,
					"kind":             "open",
					"choice_indices":   nil,
					"open_text":        "Отвечу подробно на интервью: тема мне хорошо знакома.",
					"based_on_profile": true,
					"facts_used":       []any{},
					"note":             nil,
				})
			}
		}
		v := e2eCommon(0.95)
		v["answers"] = answers
		return v
	case "warmup_ping":
		v := e2eCommon(0.9)
		v["message"] = e2ePing
		v["should_send"] = true
		v["question_form"] = true
		v["used_facts"] = []any{}
		return v
	case "vacancy_scoring":
		// T78: an obvious hit decides into the band (ok, needs_human=false);
		// a data-dummy vacancy (no title) is legitimate needs_human.
		if vac, _ := input["vacancy"].(map[string]any); vac != nil {
			if name, _ := vac["name"].(string); strings.TrimSpace(name) == "" {
				v := e2eCommon(1.0)
				v["needs_human"] = true
				v["needs_human_reason"] = "В вакансии нет названия и описания: данных для оценки физически нет"
				v["score"] = 0
				v["band"] = "weak"
				v["target_role_match"] = false
				v["reasons"] = []string{"В вакансии нет названия и описания — сравнивать не с чем"}
				v["red_flags"] = []any{}
				v["missing_data"] = []any{}
				return v
			}
		}
		v := e2eCommon(0.9)
		v["score"] = 72
		v["band"] = "ok"
		v["target_role_match"] = true
		v["reasons"] = []string{"требования вакансии покрывают подтверждённый опыт"}
		v["red_flags"] = []any{}
		v["missing_data"] = []any{}
		return v
	default:
		v := e2eCommon(0.5)
		v["needs_human"] = true
		v["needs_human_reason"] = "e2e fake: unknown task " + task
		return v
	}
}

func messageTextOf(input map[string]any) string {
	msg, _ := input["message"].(map[string]any)
	text, _ := msg["text"].(string)
	return text
}

// extractInputJSON pulls the untrusted input block (a single balanced JSON
// object) out of the user message and decodes it defensively.
func extractInputJSON(s string) map[string]any {
	start := strings.Index(s, "{")
	if start < 0 {
		return map[string]any{}
	}
	depth := 0
	inStr, esc := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				var m map[string]any
				if err := json.Unmarshal([]byte(s[start:i+1]), &m); err != nil {
					return map[string]any{}
				}
				return m
			}
		}
	}
	return map[string]any{}
}
