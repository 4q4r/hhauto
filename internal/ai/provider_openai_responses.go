package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hhauto/internal/config"
)

// OpenAI Responses API wire facts (verified against the API reference,
// 2026-09): POST {base}/responses; instructions carries the system prompt at
// top level; input is an array of {role, content:[{type:"input_text",text}]};
// reasoning:{effort} uses the same effort enum as chat; text.format
// {type:"json_schema",name,strict,schema} enables Structured Outputs;
// max_output_tokens bounds visible output plus reasoning tokens.

// OpenAIResponsesProvider speaks the openai-responses dialect.
type OpenAIResponsesProvider struct {
	base baseProvider
}

// NewOpenAIResponsesProvider builds the dialect adapter for one provider config.
func NewOpenAIResponsesProvider(id string, cfg config.AIProvider, opts ...Option) (*OpenAIResponsesProvider, error) {
	base, err := newBaseProvider(id, cfg, DialectOpenAIResponses, providerDeps(opts2deps(opts)))
	if err != nil {
		return nil, err
	}
	return &OpenAIResponsesProvider{base: base}, nil
}

// Dialect returns "openai-responses".
func (p *OpenAIResponsesProvider) Dialect() string { return DialectOpenAIResponses }

type responsesContentPart struct {
	Type string `json:"type"` // input_text | output_text
	Text string `json:"text"`
}

type responsesInputItem struct {
	Role    string                 `json:"role"`
	Content []responsesContentPart `json:"content"`
}

type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type responsesFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type responsesText struct {
	Format responsesFormat `json:"format"`
}

type responsesWireRequest struct {
	Model           string               `json:"model"`
	Instructions    string               `json:"instructions,omitempty"`
	Input           []responsesInputItem `json:"input"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Temperature     float64              `json:"temperature,omitempty"`
	Reasoning       *responsesReasoning  `json:"reasoning,omitempty"`
	Text            *responsesText       `json:"text,omitempty"`
}

type responsesWireReply struct {
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *wireError `json:"error"`
}

func (p *OpenAIResponsesProvider) responsesBody(model string, req Request, withTemperature bool) ([]byte, error) {
	w := responsesWireRequest{Model: model, Instructions: req.System}
	appendInput := func(role, text, partType string) {
		w.Input = append(w.Input, responsesInputItem{Role: role, Content: []responsesContentPart{{Type: partType, Text: text}}})
	}
	for _, m := range req.Messages {
		appendInput(m.Role, m.Content, "input_text")
	}
	if req.MaxOutputTokens > 0 {
		w.MaxOutputTokens = req.MaxOutputTokens
	}
	if withTemperature {
		w.Temperature = req.Temperature
	}
	if req.Effort != "" && req.Effort != EffortNone {
		w.Reasoning = &responsesReasoning{Effort: req.Effort}
	}
	if len(req.JSONSchema) > 0 {
		w.Text = &responsesText{Format: responsesFormat{
			Type:   "json_schema",
			Name:   schemaNameFor(req.Task),
			Strict: true,
			Schema: req.JSONSchema,
		}}
	}
	return json.Marshal(w)
}

func (p *OpenAIResponsesProvider) completeModel(ctx context.Context, model string, req Request) (Response, error) {
	if !effortAllowedOpenAI(req.Effort) {
		return Response{}, fmt.Errorf("ai: %s: unsupported reasoning effort %q", p.base.id, req.Effort)
	}
	key, err := p.base.apiKey()
	if err != nil {
		return Response{}, err
	}
	start := p.base.deps.now()
	withTemp := req.Temperature > 0 && !p.base.deps.profiles.TemperatureUnsupported(model)
	payload, err := p.responsesBody(model, req, withTemp)
	if err != nil {
		return Response{}, fmt.Errorf("ai: %s: encode request: %w", p.base.id, err)
	}
	headers := openAIHeaders(key)
	body, err := p.base.postJSON(ctx, model, "/responses", headers, payload)
	if err != nil {
		var apiErr *APIError
		if withTemp && errors.As(err, &apiErr) && apiErr.temperatureUnsupported() {
			p.base.deps.profiles.SetTemperatureUnsupported(model)
			payload, err = p.responsesBody(model, req, false)
			if err != nil {
				return Response{}, fmt.Errorf("ai: %s: encode request: %w", p.base.id, err)
			}
			body, err = p.base.postJSON(ctx, model, "/responses", headers, payload)
		}
		if err != nil {
			return Response{}, err
		}
	}
	return p.parseReply(model, body, start)
}

func (p *OpenAIResponsesProvider) parseReply(model string, body []byte, start time.Time) (Response, error) {
	var reply responsesWireReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return Response{}, fmt.Errorf("ai: %s: decode reply: %w", p.base.id, err)
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return Response{}, &APIError{ProviderID: p.base.id, Dialect: p.base.dialect, Model: model, Message: reply.Error.Message}
	}
	var content string
	for _, item := range reply.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				content += part.Text
			}
		}
	}
	if content == "" {
		return Response{}, fmt.Errorf("ai: %s: reply output carries no output_text (status=%q)", p.base.id, reply.Status)
	}
	return Response{
		Content:   content,
		Model:     model,
		TokensIn:  reply.Usage.InputTokens,
		TokensOut: reply.Usage.OutputTokens,
		LatencyMS: p.base.deps.now().Sub(start).Milliseconds(),
	}, nil
}

// Complete implements Provider; use NewBoundProvider to fix the model name.
func (p *OpenAIResponsesProvider) Complete(ctx context.Context, req Request) (Response, error) {
	return Response{}, fmt.Errorf("ai: openai-responses provider %q is not bound to a model; wrap it with NewBoundProvider", p.base.id)
}

var (
	_ Provider       = (*OpenAIResponsesProvider)(nil)
	_ modelCompleter = (*OpenAIResponsesProvider)(nil)
)
