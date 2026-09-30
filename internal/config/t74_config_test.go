package config

// T74: [ai.jev] gains the inline api_key (mirroring the T48 provider
// pattern, owner decision: no environment variables), the model name and
// the wire selector (systemone | openai-chat for OpenRouter-style chat
// backends). api_key_env stays valid for backward compatibility.

import (
	"strings"
	"testing"
)

func t74JevConfig(mut func(*AIJev)) *Config {
	c := validConfig()
	c.Features.MultiApply.Classifier = ClassifierJev
	j := AIJev{
		Enabled:             true,
		Endpoint:            "https://jev.example.com/v1/systemone",
		APIKeyEnv:           "JEV_API_KEY",
		ConfidenceThreshold: 0.9,
		Tasks:               []string{"message_classification"},
	}
	if mut != nil {
		mut(&j)
	}
	c.AI.Jev = j
	return c
}

func TestT74JevAPIKeyInline(t *testing.T) {
	// api_key inline without any environment variable — the point of the field.
	c := t74JevConfig(func(j *AIJev) { j.APIKeyEnv = ""; j.APIKey = "jev-inline-key" })
	if err := Validate(c); err != nil {
		t.Fatalf("api_key inline must validate: %v", err)
	}

	// both set is valid (api_key wins at resolve time).
	c = t74JevConfig(func(j *AIJev) { j.APIKey = "jev-inline-key" })
	if err := Validate(c); err != nil {
		t.Fatalf("api_key + api_key_env must validate: %v", err)
	}

	// neither key is a fail-fast validation error.
	c = t74JevConfig(func(j *AIJev) { j.APIKeyEnv = "" })
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("enabled jev without any key = %v, want api_key error", err)
	}

	// api_key_env only (the pre-T74 shape) stays valid.
	c = t74JevConfig(nil)
	if err := Validate(c); err != nil {
		t.Fatalf("api_key_env only must still validate: %v", err)
	}
}

func TestT74JevWireValidation(t *testing.T) {
	// unknown wire
	c := t74JevConfig(func(j *AIJev) { j.Wire = "grpc" })
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "ai.jev.wire") {
		t.Fatalf("unknown wire = %v, want ai.jev.wire error", err)
	}

	// openai-chat without a model
	c = t74JevConfig(func(j *AIJev) { j.Wire = "openai-chat" })
	err = Validate(c)
	if err == nil || !strings.Contains(err.Error(), "ai.jev.model") {
		t.Fatalf("openai-chat without model = %v, want ai.jev.model error", err)
	}

	// openai-chat happy path
	c = t74JevConfig(func(j *AIJev) {
		j.Wire = "openai-chat"
		j.Model = "~typesafe/jev-latest"
	})
	if err := Validate(c); err != nil {
		t.Fatalf("openai-chat happy path must validate: %v", err)
	}
}

func TestT74JevOpenAIChatEndpointNoDefault(t *testing.T) {
	// applyDefaults fills the systemone endpoint for the default wire...
	var c Config
	applyDefaults(&c)
	if c.AI.Jev.Endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("default endpoint = %q", c.AI.Jev.Endpoint)
	}
	if c.AI.Jev.Wire != "" || c.AI.Jev.Model != "" || c.AI.Jev.APIKey != "" {
		t.Errorf("new T74 fields must default to empty, got %+v", c.AI.Jev)
	}

	// ...but not for wire="openai-chat": the chat endpoint is explicit
	// and the enabled check reports the missing endpoint fail-fast.
	c = Config{}
	c.AI.Jev = AIJev{
		Enabled: true, Wire: "openai-chat", Model: "~typesafe/jev-latest",
		APIKey: "k", ConfidenceThreshold: 0.9,
		Tasks: []string{"message_classification"},
	}
	applyDefaults(&c)
	if c.AI.Jev.Endpoint != "" {
		t.Fatalf("openai-chat must not inherit the systemone endpoint default, got %q", c.AI.Jev.Endpoint)
	}
	if err := Validate(&c); err == nil || !strings.Contains(err.Error(), "ai.jev.endpoint") {
		t.Fatalf("openai-chat without endpoint = %v, want endpoint error", err)
	}
}
