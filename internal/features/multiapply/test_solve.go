// test_solve.go implements the T80 solve step: a fetched VacancyTest is
// answered by the AI (ai.TaskTestAnswer — the same prompt the external
// forms use) strictly from the trusted profile context, and the verdict is
// post-checked before anything reaches hh: a choice answer must resolve to
// a real solution id, a refused or empty answer is simply omitted, and any
// escalation (needs_human / injection) kills the whole solve. The caller
// translates a nil result into the T77 test_required fallback skip.

package multiapply

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
)

// defaultTestSolveTimeout bounds the whole solve (profile render + AI
// batches) when neither the config nor the Options override names a value.
const defaultTestSolveTimeout = 120 * time.Second

// maxTestBatch mirrors the test_answer batches of the external forms (≤5
// questions per AI call).
const maxTestBatch = 5

// solveContext bounds one solve: the Options override wins, then the
// config value, then the default.
func (f *Feature) solveContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := f.opt.TestSolveTimeout
	if budget <= 0 {
		if s := f.opt.Config.Features.MultiApply.TestSolveTimeoutSec; s > 0 {
			budget = time.Duration(s) * time.Second
		} else {
			budget = defaultTestSolveTimeout
		}
	}
	return context.WithTimeout(ctx, budget)
}

// solveTest answers the test and returns the submission payload, or nil
// when the solve failed (any failure means the caller skips the vacancy —
// never a partially fabricated submission).
func (f *Feature) solveTest(ctx context.Context, runID, accountID string, v hhapi.Vacancy,
	t *hhapi.VacancyTest, prof profile.Profile,
) *hhapi.ApplyTest {
	profileText := profile.RenderForPrompt(prof)

	var answers []hhapi.TestAnswer
	for start := 0; start < len(t.Tasks); start += maxTestBatch {
		if f.opt.Stopped != nil && f.opt.Stopped(ctx) {
			f.log.Info("multiapply: kill switch tripped — stopping the test solve", "vacancy", v.ID)
			return nil
		}
		end := min(start+maxTestBatch, len(t.Tasks))
		batch, err := f.answerTestBatch(ctx, runID, accountID, t.Tasks[start:end], profileText)
		if err != nil {
			f.log.Warn("multiapply: test answer batch failed", "vacancy", v.ID, "err", err)
			return nil
		}
		answers = append(answers, batch...)
	}
	if len(answers) == 0 {
		f.log.Warn("multiapply: test produced no honest answers", "vacancy", v.ID)
		return nil
	}
	return &hhapi.ApplyTest{
		UIDPk: t.UIDPk, GUID: t.GUID, StartTime: t.StartTime, Required: t.Required,
		Answers: answers,
	}
}

// testQuestion is the prompt input shape of ai.TaskTestAnswer (the same
// convention the external-form solver uses).
type testQuestion struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	Type string `json:"type,omitempty"`
}

// answerTestBatch runs one TaskTestAnswer call for up to maxTestBatch tasks
// and post-checks the verdict into submission answers. Post-checks cut
// fabrication: an unknown index, an unresolvable choice, an escalation or
// a verdict that does not cover the batch one-to-one fails the solve.
func (f *Feature) answerTestBatch(ctx context.Context, runID, accountID string,
	tasks []hhapi.TestTask, profileText string,
) ([]hhapi.TestAnswer, error) {
	questions := make([]testQuestion, 0, len(tasks))
	for i, task := range tasks {
		text := task.Question
		if len(task.Solutions) > 0 {
			opts := make([]string, 0, len(task.Solutions))
			for _, s := range task.Solutions {
				opts = append(opts, s.Text)
			}
			text += " Варианты: " + strings.Join(opts, " / ")
		}
		kind := "open"
		if len(task.Solutions) > 0 {
			kind = "choice"
		}
		questions = append(questions, testQuestion{ID: i + 1, Text: text, Type: kind})
	}
	input := map[string]any{
		"questions": questions,
		"profile":   profileText,
	}
	title := questions[0].Text[:min(40, len(questions[0].Text))]
	raw, _, err := f.opt.Runner.Run(ctx, accountID, runID, title, "", ai.TaskTestAnswer, input)
	if err != nil {
		return nil, fmt.Errorf("multiapply: ai test_answer: %w", err)
	}
	var decoded struct {
		ai.Common
		Answers []struct {
			QuestionID    string  `json:"question_id"`
			Kind          string  `json:"kind"`
			ChoiceIndices []int   `json:"choice_indices"`
			OpenText      *string `json:"open_text"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("multiapply: test verdict decode: %w", err)
	}
	if decoded.NeedsHuman || decoded.InjectionSuspected {
		return nil, fmt.Errorf("multiapply: test verdict escalated (needs_human=%t injection=%t)",
			decoded.NeedsHuman, decoded.InjectionSuspected)
	}
	if len(decoded.Answers) != len(tasks) {
		return nil, fmt.Errorf("multiapply: test verdict covers %d of %d questions", len(decoded.Answers), len(tasks))
	}
	answers := make([]hhapi.TestAnswer, 0, len(tasks))
	for i, a := range decoded.Answers {
		task := tasks[i]
		if len(task.Solutions) == 0 {
			// Open question: a short factual answer or an honest omission
			// (owner rule — no invented facts).
			if a.OpenText != nil && strings.TrimSpace(*a.OpenText) != "" {
				answers = append(answers, hhapi.TestAnswer{TaskID: task.ID, Text: strings.TrimSpace(*a.OpenText)})
			}
			continue
		}
		// Choice question: exactly one real solution, no guessing.
		if len(a.ChoiceIndices) != 1 || a.ChoiceIndices[0] < 0 || a.ChoiceIndices[0] >= len(task.Solutions) {
			return nil, fmt.Errorf("multiapply: test task %s: no valid choice (indices %v)", task.ID, a.ChoiceIndices)
		}
		answers = append(answers, hhapi.TestAnswer{TaskID: task.ID, SolutionID: task.Solutions[a.ChoiceIndices[0]].ID})
	}
	return answers, nil
}
