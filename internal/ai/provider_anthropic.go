package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"hhauto/internal/config"
)

// Anthropic Messages API wire facts (verified against docs.claude.com,
// 2026-09): POST {base}/v1/messages; the system prompt is the TOP-LEVEL
// system body field ("there is no \"system\" role for input messages");
// max_tokens is REQUIRED; auth uses x-api-key + anthropic-version headers.
// Thinking: the manual form thinking:{type:"enabled",budget_tokens} is
// deprecated on 4.6 and rejected by 4.7+ models; the current form is
// adaptive — thinking:{type:"adaptive"} with output_config.effort
// (low|medium|high|xhigh|max). Structured output: this dialect does not send
// the schema via response_format — the schema is duplicated into the system
// text and the verdict is ALWAYS validated locally by the Runner.

// AnthropicProvider speaks the anthropic-messages dialect.
type AnthropicProvider struct {
	base baseProvider
}

// NewAnthropicProvider builds the dialect adapter for one provider config.
func NewAnthropicProvider(id string, cfg config.AIProvider, opts ...Option) (*AnthropicProvider, error) {
	base, err := newBaseProvider(id, cfg, DialectAnthropicMessages, opts2deps(opts))
	if err != nil {
		return nil, err
	}
	return &AnthropicProvider{base: base}, nil
}

// Dialect returns "anthropic-messages".
func (p *AnthropicProvider) Dialect() string { return DialectAnthropicMessages }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicThinking struct {
	Type string `json:"type"` // "adaptive"
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicWireRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Temperature  float64                `json:"temperature,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicWireReply struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason string     `json:"stop_reason"`
	Error      *wireError `json:"error"`
}

// anthropicEffort maps the cross-dialect effort enum onto the values the
// Messages API accepts in output_config.effort.
func anthropicEffort(effort string) (string, bool, error) {
	switch effort {
	case "", EffortNone:
		return "", false, nil
	case "minimal":
		return "low", true, nil
	case "low", "medium", "high", "xhigh", "max":
		return effort, true, nil
	}
	return "", false, fmt.Errorf("ai: anthropic: unsupported effort %q", effort)
}

func (p *AnthropicProvider) body(model string, req Request, withTemperature, withThinking bool) ([]byte, error) {
	effort, thinking, err := anthropicEffort(req.Effort)
	if err != nil {
		return nil, err
	}
	w := anthropicWireRequest{Model: model, System: req.System}
	if req.MaxOutputTokens > 0 {
		w.MaxTokens = req.MaxOutputTokens
	} else {
		w.MaxTokens = defaultAnthropicMaxTokens
	}
	for _, m := range req.Messages {
		w.Messages = append(w.Messages, anthropicMessage(m))
	}
	if withThinking && thinking {
		w.Thinking = &anthropicThinking{Type: "adaptive"}
		w.OutputConfig = &anthropicOutputConfig{Effort: effort}
		// Thinking modes and temperature sampling are not combinable; the
		// effort control replaces sampling tuning for reasoning requests.
	} else if withTemperature {
		w.Temperature = min(req.Temperature, 1.0)
	}
	// Structured output per dialect contract: duplicate the schema into the
	// system text; the Runner always validates locally afterwards.
	if len(req.JSONSchema) > 0 {
		w.System += "\n\n# ФОРМАТ ОТВЕТА (JSON SCHEMA)\n" +
			"Верни ровно один JSON-объект, точно соответствующий этой схеме:\n" +
			string(req.JSONSchema)
	}
	return json.Marshal(w)
}

func (p *AnthropicProvider) headers(key string) map[string]string {
	return map[string]string{
		"x-api-key":         key,
		"anthropic-version": anthropicVersion,
	}
}

func (p *AnthropicProvider) completeModel(ctx context.Context, model string, req Request) (Response, error) {
	key, err := p.base.apiKey()
	if err != nil {
		return Response{}, err
	}
	start := p.base.deps.now()
	withTemp := req.Temperature > 0 && !p.base.deps.profiles.TemperatureUnsupported(model)
	withThinking := true
	payload, err := p.body(model, req, withTemp, withThinking)
	if err != nil {
		return Response{}, fmt.Errorf("ai: %s: encode request: %w", p.base.id, err)
	}
	headers := p.headers(key)
	body, err := p.base.postJSON(ctx, model, "/v1/messages", headers, payload)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.thinkingUnsupported() {
			// Model does not accept adaptive thinking/effort controls:
			// retry once without them (max_tokens stays — it is required).
			withThinking = false
			payload, err = p.body(model, req, withTemp, withThinking)
			if err != nil {
				return Response{}, fmt.Errorf("ai: %s: encode request: %w", p.base.id, err)
			}
			body, err = p.base.postJSON(ctx, model, "/v1/messages", headers, payload)
		}
		if err != nil {
			return Response{}, err
		}
	}
	return p.parseReply(model, body, start)
}

func (p *AnthropicProvider) parseReply(model string, body []byte, start time.Time) (Response, error) {
	var reply anthropicWireReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return Response{}, fmt.Errorf("ai: %s: decode reply: %w", p.base.id, err)
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return Response{}, &APIError{ProviderID: p.base.id, Dialect: p.base.dialect, Model: model, Message: reply.Error.Message}
	}
	var content strings.Builder
	for _, block := range reply.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}
	if content.Len() == 0 {
		return Response{}, fmt.Errorf("ai: %s: reply carries no text blocks (stop_reason=%q)", p.base.id, reply.StopReason)
	}
	return Response{
		Content:   content.String(),
		Model:     model,
		TokensIn:  reply.Usage.InputTokens,
		TokensOut: reply.Usage.OutputTokens,
		LatencyMS: p.base.deps.now().Sub(start).Milliseconds(),
	}, nil
}

// Complete implements Provider; use NewBoundProvider to fix the model name.
func (p *AnthropicProvider) Complete(_ context.Context, _ Request) (Response, error) {
	return Response{}, fmt.Errorf("ai: anthropic provider %q is not bound to a model; wrap it with NewBoundProvider", p.base.id)
}

var (
	_ Provider       = (*AnthropicProvider)(nil)
	_ modelCompleter = (*AnthropicProvider)(nil)
)
