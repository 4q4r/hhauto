package setka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features/kit11"
	"hhauto/internal/hhapi"
	"hhauto/internal/setka"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// jsonMarshal is json.Marshal with the error wrapped for park().
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// candidate is one target-filtered person with its classification.
type candidate struct {
	person     setka.Person
	confidence float64
}

// filterAndGate applies the target pre-filters (blacklists, status,
// headline) and the can-message gate ("призраки" из hh писать нельзя).
// Transport errors abort the flow; filtered people just drop out.
func (f *Feature) filterAndGate(ctx context.Context, client setka.SetkaClient, cfg *config.Setka, employerName string, people []setka.Person) ([]setka.Person, error) {
	requireCanMessage := cfg.RequireCanMessage == nil || *cfg.RequireCanMessage
	statusFilter := map[string]bool{}
	for _, s := range cfg.Targets.StatusFilter {
		statusFilter[strings.TrimSpace(s)] = true
	}
	out := make([]setka.Person, 0, len(people))
	for _, p := range people {
		if blacklisted(cfg.BlacklistPersons, p.ID) {
			continue
		}
		if blacklistedSubstring(cfg.BlacklistCompanies, p.Company) ||
			blacklistedSubstring(cfg.BlacklistCompanies, employerName) {
			continue
		}
		if len(statusFilter) > 0 && !statusFilter[strings.TrimSpace(p.Status)] {
			continue
		}
		if len(cfg.Targets.HeadlineInclude) > 0 && !anySubstring(cfg.Targets.HeadlineInclude, p.Headline) {
			continue
		}
		if anySubstring(cfg.Targets.HeadlineExclude, p.Headline) {
			continue
		}
		if requireCanMessage {
			ok, err := client.CanMessage(ctx, p.ID)
			if err != nil {
				return nil, fmt.Errorf("setka: can_message %s: %w", p.ID, err)
			}
			if !ok {
				f.log().Info("setka: пропуск «призрака» (писать нельзя)", "person", p.ID)
				continue
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func blacklisted(list []string, id string) bool {
	for _, b := range list {
		if strings.TrimSpace(b) == id {
			return true
		}
	}
	return false
}

func blacklistedSubstring(list []string, s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	for _, b := range list {
		if b != "" && strings.Contains(low, strings.ToLower(b)) {
			return true
		}
	}
	return false
}

func anySubstring(list []string, s string) bool {
	low := strings.ToLower(s)
	for _, sub := range list {
		if sub != "" && strings.Contains(low, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// classifyAll runs the AI verdict per candidate and returns the survivors
// (apply, or borderline per the policy) sorted by confidence desc.
func (f *Feature) classifyAll(ctx context.Context, p DMPayload, cfg *config.Setka, vacancy *hhapi.Vacancy, people []setka.Person) ([]candidate, error) {
	out := make([]candidate, 0, len(people))
	for _, person := range people {
		raw, _, err := f.AI.Run(ctx, p.AccountID, "setka_person", person.ID, p.RunID,
			ai.TaskSetkaClassify, classifyInput{Vacancy: vacancy, Person: person})
		if err != nil {
			return nil, fmt.Errorf("setka: classify %s: %w", person.ID, err)
		}
		var v struct {
			ai.Common
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("setka: classify %s verdict: %w", person.ID, err)
		}
		if v.NeedsHuman || v.InjectionSuspected {
			if err := f.park(ctx, p.AccountID, "setka_classify_unsure",
				fmt.Sprintf("setka: классификация %s требует человека", person.ID),
				map[string]any{"person": person.ID, "reason": v.NeedsHumanReason}, p, false); err != nil {
				return nil, err
			}
			continue
		}
		switch v.Decision {
		case "apply":
			out = append(out, candidate{person: person, confidence: v.Confidence})
		case "borderline":
			if cfg.Classify.Borderline == config.SetkaBorderlineApply {
				out = append(out, candidate{person: person, confidence: v.Confidence})
			}
		default: // "skip" and anything unrecognized
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].confidence > out[j].confidence })
	return out, nil
}

// classifyInput is the setka_classify task input: the full vacancy dump and
// the person card.
type classifyInput struct {
	Vacancy *hhapi.Vacancy `json:"vacancy"`
	Person  setka.Person   `json:"person"`
}

// personalizeInput is the setka_personalize task input: the full vacancy
// dump, the person's full profile and the apply context.
type personalizeInput struct {
	Vacancy     *hhapi.Vacancy     `json:"vacancy"`
	Person      setka.Person       `json:"person"`
	Application personalizeContext `json:"application"`
}

type personalizeContext struct {
	AppliedAt string `json:"applied_at"`
	ResumeID  string `json:"resume_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
}

// DMReviewPayload is the parked payload of a kind=setka_dm review item (the
// approve flow); the resolve command sends exactly this.
type DMReviewPayload struct {
	DMID         int64  `json:"dm_id"`
	PersonID     string `json:"person_id"`
	PersonName   string `json:"person_name,omitempty"`
	Company      string `json:"company,omitempty"`
	VacancyID    int64  `json:"vacancy_id"`
	VacancyTitle string `json:"vacancy_title,omitempty"`
	Text         string `json:"text"`
	Application  string `json:"application"`
}

// ParseDMReviewPayload decodes a setka_dm review payload; nil for other
// kinds or shapes.
func ParseDMReviewPayload(raw string) *DMReviewPayload {
	var p DMReviewPayload
	if json.Unmarshal([]byte(raw), &p) != nil || p.PersonID == "" || p.Text == "" || p.DMID == 0 {
		return nil
	}
	return &p
}

// messageTop personalizes and delivers the DMs of the top candidates under
// the per-company, per-day and per-person caps.
func (f *Feature) messageTop(ctx context.Context, client setka.SetkaClient, cfg *config.Setka, p DMPayload, vacancy *hhapi.Vacancy, scored []candidate) error {
	if len(scored) == 0 {
		return nil
	}
	if cfg.MaxDMPerCompany > 0 && len(scored) > cfg.MaxDMPerCompany {
		scored = scored[:cfg.MaxDMPerCompany]
	}
	day := kit11.Day(f.now())
	daySent, err := f.DB.SetkaDMsCountDay(ctx, p.AccountID, day)
	if err != nil {
		return fmt.Errorf("setka: day count: %w", err)
	}
	companySent, err := f.DB.SetkaDMsCountDayByEmployer(ctx, p.AccountID, p.EmployerID, day)
	if err != nil {
		return fmt.Errorf("setka: company count: %w", err)
	}
	for _, c := range scored {
		stop, err := f.deliverOne(ctx, client, cfg, p, vacancy, c, &daySent, &companySent)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// capHit applies the day / company / person send caps. stop=true with a
// day or company cap means the pass is over (the log line explains);
// stop=true with the person cap skips just this candidate.
func (f *Feature) capHit(ctx context.Context, cfg *config.Setka, p DMPayload, c candidate,
	day string, daySent, companySent *int,
) (stop bool, err error) {
	if cfg.MaxDMPerDay > 0 && *daySent >= cfg.MaxDMPerDay {
		f.log().Info("setka: дневной лимит исчерпан, остальные адресаты пропущены", "application", p.DedupKey)
		return true, nil
	}
	if cfg.MaxDMPerCompany > 0 && *companySent >= cfg.MaxDMPerCompany {
		f.log().Info("setka: лимит по компании исчерпан", "application", p.DedupKey)
		return true, nil
	}
	if cfg.MaxDMPerPerson > 0 {
		n, err := f.DB.SetkaDMsCountDayByPerson(ctx, p.AccountID, c.person.ID, day)
		if err != nil {
			return false, fmt.Errorf("setka: person count: %w", err)
		}
		if n >= cfg.MaxDMPerPerson {
			return true, nil
		}
	}
	return false, nil
}

// dmPayload builds the review payload of one parked DM.
func dmPayload(rowID int64, p DMPayload, vacancy *hhapi.Vacancy, personID, personName, text string) DMReviewPayload {
	return DMReviewPayload{
		DMID: rowID, PersonID: personID, PersonName: personName,
		Company: vacancy.EmployerName, VacancyID: p.VacancyID,
		VacancyTitle: vacancy.Name, Text: text, Application: p.DedupKey,
	}
}

// deliverOne runs the personalize → validate → deliver sequence for one
// candidate. stop=true means a cap was hit and the remaining candidates are
// skipped for this pass; (false, nil) means the candidate was handled
// (sent, parked or skipped) and the loop continues.
func (f *Feature) deliverOne(ctx context.Context, client setka.SetkaClient, cfg *config.Setka,
	p DMPayload, vacancy *hhapi.Vacancy, c candidate, daySent, companySent *int,
) (stop bool, err error) {
	day := kit11.Day(f.now())
	stop, err = f.capHit(ctx, cfg, p, c, day, daySent, companySent)
	if err != nil || stop {
		return stop, err
	}
	ok, err := f.existsRow(ctx, p.AccountID, c.person.ID, p.VacancyID)
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil // идемпотентность: этому человеку по этой вакансии уже писали
	}
	full := c.person
	if fp, err := client.GetPerson(ctx, c.person.ID); err != nil {
		return false, fmt.Errorf("setka: get_person %s: %w", c.person.ID, err)
	} else if fp != nil {
		full = *fp
	}
	text, ok := f.personalize(ctx, p, cfg, vacancy, full)
	if !ok {
		return false, nil // parked for review inside
	}
	if cfg.Mode == config.SetkaModeAuto && f.Cfg.General.DryRun {
		f.log().Info("setka: dry-run, отправка пропущена", "person", c.person.ID, "text", text)
		return false, nil
	}
	if cfg.NoLinks == nil || *cfg.NoLinks {
		if kit11.HasLink(text) {
			return false, f.parkLinksViolation(ctx, cfg, p, vacancy, full, text)
		}
	}
	rowID, err := f.DB.SetkaDMCreate(ctx, &store.SetkaDM{
		AccountID: p.AccountID, ApplicationDedupKey: p.DedupKey,
		VacancyID: p.VacancyID, EmployerID: p.EmployerID,
		PersonID: c.person.ID, PersonName: full.Name, Company: vacancy.EmployerName,
		Mode: cfg.Mode, Status: "queued_review", Text: text,
	})
	if err != nil {
		return false, fmt.Errorf("setka: ledger create: %w", err)
	}
	if cfg.Mode == config.SetkaModeApprove {
		if err := f.parkApprove(ctx, p, vacancy, rowID, c.person.ID, full.Name, text); err != nil {
			return false, err
		}
		*daySent++
		*companySent++
		return false, nil
	}
	// auto mode
	return false, f.sendAuto(ctx, client, p, vacancy, rowID, c.person.ID, full.Name, text, daySent, companySent)
}

// sendAuto performs the mode=auto delivery: send → mark sent → metric →
// webhook. A not-captured endpoint aborts loudly; a transport failure parks
// the outcome for the human.
func (f *Feature) sendAuto(ctx context.Context, client setka.SetkaClient,
	p DMPayload, vacancy *hhapi.Vacancy, rowID int64, personID, personName, text string,
	daySent, companySent *int,
) error {
	if err := client.SendDM(ctx, personID, text); err != nil {
		if errors.Is(err, setka.ErrEndpointNotCaptured) {
			return fmt.Errorf("setka: send %s: %w", personID, err) // loud config error
		}
		return f.parkSendFailure(ctx, p, vacancy, rowID, personID, personName, text, err)
	}
	if err := f.DB.SetkaDMMarkSent(ctx, rowID); err != nil {
		return fmt.Errorf("setka: mark sent: %w", err)
	}
	if err := f.DB.MetricsIncr(ctx, "setka.dm", kit11.Day(f.now())); err != nil {
		return fmt.Errorf("setka: metric: %w", err)
	}
	*daySent++
	*companySent++
	if f.Notify != nil {
		f.Notify.Emit(webhook.Event{
			Type: webhook.EvSetkaDMSent, AccountID: p.AccountID, Time: f.now(),
			Message: "ЛС в Сетке отправлено",
			Payload: map[string]any{
				"person_id": personID, "vacancy_id": p.VacancyID,
				"company": vacancy.EmployerName,
			},
		})
	}
	return nil
}

// parkLinksViolation records a no_links validation failure: a skipped
// ledger row plus a review item carrying the full text (the human decides).
func (f *Feature) parkLinksViolation(ctx context.Context, cfg *config.Setka, p DMPayload, vacancy *hhapi.Vacancy, person setka.Person, text string) error {
	rowID, err := f.DB.SetkaDMCreate(ctx, &store.SetkaDM{
		AccountID: p.AccountID, ApplicationDedupKey: p.DedupKey,
		VacancyID: p.VacancyID, EmployerID: p.EmployerID,
		PersonID: person.ID, PersonName: person.Name, Company: vacancy.EmployerName,
		Mode: cfg.Mode, Status: "skipped", Text: text, Reason: strPtr("ссылки в тексте запрещены (no_links)"),
	})
	if err != nil {
		return fmt.Errorf("setka: ledger create: %w", err)
	}
	return f.park(ctx, p.AccountID, "setka_dm_links",
		"setka: в тексте ЛС ссылка — валидационная ошибка, отправка заблокирована",
		dmPayload(rowID, p, vacancy, person.ID, person.Name, text), p, false)
}

// parkSendFailure records a failed DM send: the row is marked skipped (no
// auto-retry — the wire contract has no idempotency key yet) and the human
// sees the outcome.
func (f *Feature) parkSendFailure(ctx context.Context, p DMPayload, vacancy *hhapi.Vacancy,
	rowID int64, personID, personName, text string, sendErr error,
) error {
	if _, merr := f.DB.SetkaDMGet(ctx, rowID); merr == nil {
		if serr := f.DB.SetkaDMMarkSkipped(ctx, rowID, "send failed: "+sendErr.Error()); serr != nil {
			return serr
		}
	}
	return f.park(ctx, p.AccountID, "setka_send_failed",
		fmt.Sprintf("setka: отправка ЛС %s не прошла", personID),
		dmPayload(rowID, p, vacancy, personID, personName, text), p, false)
}

// existsRow is the person-per-vacancy idempotency check.
func (f *Feature) existsRow(ctx context.Context, accountID, personID string, vacancyID int64) (bool, error) {
	ok, err := f.DB.SetkaDMExists(ctx, accountID, personID, vacancyID)
	if err != nil {
		return false, fmt.Errorf("setka: exists: %w", err)
	}
	return ok, nil
}

// parkApprove binds the queued ledger row to a review item (kind=setka_dm);
// `hhauto review resolve <id> --decision apply` sends it.
func (f *Feature) parkApprove(ctx context.Context, p DMPayload, vacancy *hhapi.Vacancy, rowID int64, personID, personName, text string) error {
	payload := DMReviewPayload{
		DMID: rowID, PersonID: personID, PersonName: personName,
		Company: vacancy.EmployerName, VacancyID: p.VacancyID,
		VacancyTitle: vacancy.Name, Text: text, Application: p.DedupKey,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("setka: payload: %w", err)
	}
	var reviewID int64
	if f.Review != nil {
		reviewID, err = f.Review.Add(ctx, p.AccountID, ReviewKindSetkaDM,
			"setka: ЛС готово — требуется подтверждение отправки (режим approve)", string(b))
	} else {
		reviewID, err = f.DB.ReviewItemsAdd(ctx, p.AccountID, ReviewKindSetkaDM,
			"setka: ЛС готово — требуется подтверждение отправки (режим approve)", string(b))
	}
	if err != nil {
		return fmt.Errorf("setka: review add: %w", err)
	}
	if err := f.DB.SetkaDMSetReview(ctx, rowID, reviewID); err != nil {
		return fmt.Errorf("setka: bind review: %w", err)
	}
	return nil
}

// personalize produces the DM text: the AI verdict, or the configured
// template fallback when the AI is unavailable. ok=false means parked for
// review (nothing to send).
func (f *Feature) personalize(ctx context.Context, p DMPayload, cfg *config.Setka, vacancy *hhapi.Vacancy, person setka.Person) (string, bool) {
	raw, _, err := f.AI.Run(ctx, p.AccountID, "setka_person", person.ID, p.RunID,
		ai.TaskSetkaPersonalize, personalizeInput{
			Vacancy: vacancy, Person: person,
			Application: personalizeContext{
				AppliedAt: f.now().UTC().Format(time.RFC3339),
				ResumeID:  p.ResumeID, RunID: p.RunID,
			},
		})
	if err == nil {
		var v struct {
			ai.Common
			Message string `json:"message"`
		}
		jerr := json.Unmarshal(raw, &v)
		switch {
		case jerr == nil && (v.NeedsHuman || v.InjectionSuspected || strings.TrimSpace(v.Message) == ""):
			_ = f.park(ctx, p.AccountID, "setka_personalize_unsure",
				fmt.Sprintf("setka: черновик ЛС для %s требует человека", person.ID),
				map[string]any{"person": person.ID, "reason": v.NeedsHumanReason}, p, false)
			return "", false
		case jerr == nil:
			return strings.TrimSpace(v.Message), true
		default:
			err = jerr
		}
	}
	// AI unavailable/broken → the configured template fallback.
	if cfg.Personalize.TemplateFallback != "" {
		text, terr := renderTemplate(cfg.Personalize.TemplateFallback, map[string]string{
			"PersonName":   person.Name,
			"VacancyTitle": vacancy.Name,
			"CompanyName":  vacancy.EmployerName,
		})
		if terr == nil && strings.TrimSpace(text) != "" {
			f.log().Info("setka: ИИ недоступен — использован template_fallback", "person", person.ID)
			return strings.TrimSpace(text), true
		}
		f.log().Error("setka: template_fallback не отрендерился", "err", terr)
	}
	reason := "ИИ недоступен"
	if err != nil {
		reason = "ИИ недоступен: " + err.Error()
	}
	_ = f.park(ctx, p.AccountID, "setka_ai_failed",
		fmt.Sprintf("setka: не удалось получить черновик ЛС для %s", person.ID),
		map[string]any{"person": person.ID, "reason": reason}, p, false)
	return "", false
}

// renderTemplate renders the fallback template over the placeholder map.
func renderTemplate(tpl string, data map[string]string) (string, error) {
	t, err := template.New("setka_fallback").Parse(tpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func strPtr(s string) *string { return &s }
