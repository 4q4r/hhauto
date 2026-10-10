package ai

// T119 claim/tenure anti-fabrication: dictionary technologies attributed to
// the candidate and «N лет с X» tenure pairs must be grounded in the trusted
// sources (T113 §6). Honest markers («работал с», «освоить») and vacancy
// requirement quotes («требуется») are exempt — flagging them would cut
// valid letters.

import (
	"context"
	"encoding/json"
	"testing"

	"hhauto/internal/config"
)

func TestClaimLeakTable(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		trusted string
		want    bool
	}{
		// positives: attribution without a profile fact
		{"strong verb", "Владею Kubernetes и Kafka.", "", true},
		{"experience claim", "Имею опыт с Terraform.", "docker, nginx", true},
		{"mixed with fact", "Разрабатываю на Go, владею Airflow.", "Go", true},
		// negatives: honest markers, vacancy quotes, grounded tech
		{"worked-with marker", "Работал с PostgreSQL и RabbitMQ.", "PostgreSQL", false},
		{"vacancy quote", "В вакансии требуется Kubernetes.", "", false},
		{"your-stack quote", "Ваш стек на React — учту.", "", false},
		{"future learning", "Готов быстро освоить Airflow.", "", false},
		{"acquainted hedge", "Знаком со Spark.", "", false},
		{"grounded tech", "Разрабатываю на Go.", "опыт 4 года Go", false},
		{"no techs", "Готов обсудить позицию.", "", false},
		{"alias grounded", "Пишу на Golang.", "стек: Go, Redis", false},
	}
	for _, tc := range cases {
		if got := claimLeak(tc.text, tc.trusted); got != tc.want {
			t.Errorf("%s: claimLeak(%q, %q) = %v, want %v", tc.name, tc.text, tc.trusted, got, tc.want)
		}
	}
}

func TestTenureLeakTable(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		trusted string
		want    bool
	}{
		// positives: pair absent from the trusted sources
		{"pair absent", "5 лет с Kubernetes.", "10 лет с Kubernetes", true},
		{"years mismatch", "3 года Go.", "5 лет Go", true},
		{"tech-before-years", "Kubernetes — 5 лет.", "", true},
		// negatives: pair grounded or vacancy quote
		{"pair present", "5 лет с Kubernetes.", "опыт: Kubernetes 5 лет", false},
		{"both orders", "Go 4 года.", "4 года Go, PostgreSQL", false},
		{"vacancy quote", "В вакансии требуется Kubernetes 3 года.", "", false},
		{"no pair", "3 года коммерческой разработки.", "", false},
	}
	for _, tc := range cases {
		if got := tenureLeak(tc.text, tc.trusted); got != tc.want {
			t.Errorf("%s: tenureLeak(%q, %q) = %v, want %v", tc.name, tc.text, tc.trusted, got, tc.want)
		}
	}
}

// Tenure exemption is narrower than the claim one: «работал с» does not
// excuse a (technology, years) pair — the pair itself must be in the
// profile.
func TestTenureLeakWorkedWithNotExempt(t *testing.T) {
	if !tenureLeak("Работал с Kubernetes 5 лет.", "5 лет с PostgreSQL") {
		t.Error("worked-with tenure pair absent from sources must flag")
	}
}

func TestRunCoverLetterClaimNotInProfile(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Здравствуйте! Владею Kubernetes и Terraform, готов решать сложные задачи.","mode":"ai_company","used_facts":[],"missing_data":[]}`}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, err := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"resume_digest": "опыт 4 года Go",
		"profile_text":  "ищу удалёнку",
		"facts":         []string{},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman {
		t.Errorf("attributed technology must force needs_human, decision = %+v", dec)
	}
	if reasonOf(verdict) != "claim_not_in_profile" {
		t.Errorf("reason = %q, want claim_not_in_profile", reasonOf(verdict))
	}
}

func TestRunCoverLetterClaimMarkersPass(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"В вакансии требуется Kubernetes. Работал с Go.","mode":"ai_company","used_facts":[],"missing_data":[]}`}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"resume_digest": "опыт 4 года Go",
		"profile_text":  "ищу удалёнку",
		"facts":         []string{},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec.NeedsHuman {
		t.Errorf("honest markers flagged: %+v, verdict %s", dec, verdict)
	}
}

func TestRunCoverLetterTenureNotInSources(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Работаю активно, 5 лет с Kubernetes, приносил результат команде.","mode":"ai_company","used_facts":[],"missing_data":[]}`}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"resume_digest": "Kubernetes-опыт, 5 лет с PostgreSQL",
		"profile_text":  "",
		"facts":         []string{},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman {
		t.Errorf("ungrounded tenure pair must force needs_human, decision = %+v", dec)
	}
	if reasonOf(verdict) != "tenure_not_in_sources" {
		t.Errorf("reason = %q, want tenure_not_in_sources", reasonOf(verdict))
	}
}

func TestRunChatReplyClaimNotInProfile(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"reply":"Имею опыт с Kafka.","intent":"ack","used_facts":[]}`}}
	tasks := map[string]config.AITask{"chat_reply": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"chat":  []any{},
		"facts": []string{"ищу удалёнку"},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "chat", "1", "r", TaskChatReply, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman || reasonOf(verdict) != "claim_not_in_profile" {
		t.Errorf("verdict=%s dec=%+v, want claim_not_in_profile on the chat surface", verdict, dec)
	}
}

// The chatops production input carries the profile under the "profile" key;
// trustedSources must read it so grounded tech mentions in replies pass.
func TestRunChatReplyProfileGroundedPasses(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"reply":"Работаю с Go и Kubernetes.","intent":"ack","used_facts":[]}`}}
	tasks := map[string]config.AITask{"chat_reply": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"chat":    []any{},
		"profile": "стек: Go, Kubernetes",
	}
	verdict, dec, err := r.Run(context.Background(), "a", "chat", "1", "r", TaskChatReply, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec.NeedsHuman {
		t.Errorf("profile-grounded reply flagged: %+v, verdict %s", dec, verdict)
	}
}

func TestRunImproveClaimNotInProfile(t *testing.T) {
	p := &scriptedProvider{contents: []string{improveVerdict(t, map[string]any{
		"value_string": "Владею Kubernetes.",
	})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	verdict, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "sys", ImproveInput{
			Field:        "about",
			TrustedFacts: []string{"Go, PostgreSQL"},
		})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if !dec.NeedsHuman {
		t.Fatalf("attributed technology must force needs_human, verdict=%s", verdict)
	}
	var out struct {
		Reason string `json:"needs_human_reason"`
	}
	_ = json.Unmarshal(verdict, &out)
	if out.Reason != "claim_not_in_profile" {
		t.Fatalf("reason = %q, want claim_not_in_profile", out.Reason)
	}
}

func TestRunImproveTenureNotInSources(t *testing.T) {
	p := &scriptedProvider{contents: []string{improveVerdict(t, map[string]any{
		"value_string": "5 лет с Kubernetes.",
	})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	verdict, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "sys", ImproveInput{
			Field:        "about",
			TrustedFacts: []string{"Kubernetes-опыт, 5 лет с PostgreSQL"},
		})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if !dec.NeedsHuman {
		t.Fatalf("ungrounded tenure pair must force needs_human, verdict=%s", verdict)
	}
	var out struct {
		Reason string `json:"needs_human_reason"`
	}
	_ = json.Unmarshal(verdict, &out)
	if out.Reason != "tenure_not_in_sources" {
		t.Fatalf("reason = %q, want tenure_not_in_sources", out.Reason)
	}
}
