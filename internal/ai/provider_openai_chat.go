package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hhauto/internal/config"
)

// OpenAI Chat Completions wire facts (verified against the API reference,
// 2026-09): POST {base}/chat/completions; max_completion_tokens replaces the
// deprecated max_tokens (incompatible with o-series models); reasoning_effort
// takes none|minimal|low|medium|high|xhigh|max; response_format
// {"type":"json_schema","json_schema":{name,strict,schema}} enables strict
// Structured Outputs.

// OpenAIChatProvider speaks the openai-chat dialect.
type OpenAIChatProvider struct {
	base baseProvider
}

// NewOpenAIChatProvider builds the dialect adapter for one provider config.
func NewOpenAIChatProvider(id string, cfg config.AIProvider, opts ...Option) (*OpenAIChatProvider, error) {
	base, err := newBaseProvider(id, cfg, DialectOpenAIChat, opts2deps(opts))
	if err != nil {
		return nil, err
	}
	return &OpenAIChatProvider{base: base}, nil
}

// Dialect returns "openai-chat".
func (p *OpenAIChatProvider) Dialect() string { return DialectOpenAIChat }

type chatWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type strictJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type jsonSchemaResponseFormat struct {
	Type       string           `json:"type"`
	JSONSchema strictJSONSchema `json:"json_schema"`
}

type chatWireRequest struct {
	Model               string                    `json:"model"`
	Messages            []chatWireMessage         `json:"messages"`
	MaxCompletionTokens int                       `json:"max_completion_tokens,omitempty"`
	Temperature         float64                   `json:"temperature,omitempty"`
	ReasoningEffort     string                    `json:"reasoning_effort,omitempty"`
	ResponseFormat      *jsonSchemaResponseFormat `json:"response_format,omitempty"`
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type chatWireReply struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *wireError `json:"error"`
}

func opts2deps(opts []Option) providerDeps {
	var d providerDeps
	for _, o := range opts {
		o(&d)
	}
	return d
}

func (p *OpenAIChatProvider) chatBody(model string, req Request, withTemperature bool) ([]byte, error) {
	w := chatWireRequest{Model: model}
	if req.System != "" {
		w.Messages = append(w.Messages, chatWireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		w.Messages = append(w.Messages, chatWireMessage(m))
	}
	if req.MaxOutputTokens > 0 {
		w.MaxCompletionTokens = req.MaxOutputTokens
	}
	if withTemperature {
		w.Temperature = req.Temperature
	}
	if req.Effort != "" && req.Effort != EffortNone {
		w.ReasoningEffort = req.Effort
	}
	if len(req.JSONSchema) > 0 {
		w.ResponseFormat = &jsonSchemaResponseFormat{
			Type: "json_schema",
			JSONSchema: strictJSONSchema{
				Name:   schemaNameFor(req.Task),
				Strict: true,
				Schema: req.JSONSchema,
			},
		}
	}
	return json.Marshal(w)
}

func (p *OpenAIChatProvider) completeModel(ctx context.Context, model string, req Request) (Response, error) {
	if !effortAllowedOpenAI(req.Effort) {
		return Response{}, fmt.Errorf("ai: %s: unsupported reasoning_effort %q", p.base.id, req.Effort)
	}
	key, err := p.base.apiKey()
	if err != nil {
		return Response{}, err
	}
	start := p.base.deps.now()
	withTemp := req.Temperature > 0 && !p.base.deps.profiles.TemperatureUnsupported(model)
	payload, err := p.chatBody(model, req, withTemp)
	if err != nil {
		return Response{}, fmt.Errorf("ai: %s: encode request: %w", p.base.id, err)
	}
	headers := openAIHeaders(key)
	body, err := p.base.postJSON(ctx, model, "/chat/completions", headers, payload)
	if err != nil {
		var apiErr *APIError
		if withTemp && errors.As(err, &apiErr) && apiErr.temperatureUnsupported() {
			// Drop temperature, remember the quirk, retry once with a full
			// retry budget.
			p.base.deps.profiles.SetTemperatureUnsupported(model)
			payload, err = p.chatBody(model, req, false)
			if err != nil {
				return Response{}, fmt.Errorf("ai: %s: encode request: %w", p.base.id, err)
			}
			body, err = p.base.postJSON(ctx, model, "/chat/completions", headers, payload)
		}
		if err != nil {
			return Response{}, err
		}
	}
	return parseChatReply(p.base, model, body, start)
}

func parseChatReply(base baseProvider, model string, body []byte, start time.Time) (Response, error) {
	var reply chatWireReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return Response{}, fmt.Errorf("ai: %s: decode reply: %w", base.id, err)
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return Response{}, &APIError{ProviderID: base.id, Dialect: base.dialect, Model: model, Message: reply.Error.Message}
	}
	if len(reply.Choices) == 0 {
		return Response{}, fmt.Errorf("ai: %s: reply carries no choices", base.id)
	}
	content := reply.Choices[0].Message.Content
	if content == "" {
		return Response{}, fmt.Errorf("ai: %s: reply content is empty (finish_reason=%q)", base.id, reply.Choices[0].FinishReason)
	}
	return Response{
		Content:   content,
		Model:     model,
		TokensIn:  reply.Usage.PromptTokens,
		TokensOut: reply.Usage.CompletionTokens,
		LatencyMS: base.deps.now().Sub(start).Milliseconds(),
	}, nil
}

// Complete implements Provider; use NewBoundProvider to fix the model name.
func (p *OpenAIChatProvider) Complete(_ context.Context, _ Request) (Response, error) {
	return Response{}, fmt.Errorf("ai: openai-chat provider %q is not bound to a model; wrap it with NewBoundProvider", p.base.id)
}

// compile-time interface checks
var (
	_ Provider       = (*OpenAIChatProvider)(nil)
	_ modelCompleter = (*OpenAIChatProvider)(nil)
)
