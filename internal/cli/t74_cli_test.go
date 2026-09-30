package cli

// T74 wiring: [ai.jev].api_key resolves the Bearer key from the config
// file (the T48 pattern) with api_key_env as the fallback; an enabled
// classifier with neither fails fast at startup, and the T74 transport
// options (wire/model/headers) reach the client.

import (
	"io"
	"log/slog"
	"testing"

	"hhauto/internal/config"
)

func t74JevCLIConfig(jev config.AIJev) *config.Config {
	jev.Enabled = true
	jev.Endpoint = "https://jev.example.com/v1/systemone"
	jev.ConfidenceThreshold = 0.9
	jev.Tasks = []string{"message_classification"}
	return &config.Config{
		Features: config.Features{
			AutoClone: config.AutoClone{Improve: config.AutoCloneImprove{
				Enabled: true, Fields: []string{"title"}, ModelProfile: "improve",
			}},
			MultiApply: config.MultiApply{Classifier: config.ClassifierJev},
		},
		AI: config.AI{
			Providers: map[string]config.AIProvider{"zai": {
				Dialect: "openai-chat", BaseURL: "https://api.example.com/v4", APIKey: "pk",
			}},
			Models: map[string]config.AIModel{"improve": {Provider: "zai", Model: "glm"}},
			Jev:    jev,
		},
	}
}

func TestT74BuildAIJevAPIKeyFromConfig(t *testing.T) {
	// No JEV_API_KEY in the environment: the config key must be enough.
	cfg := t74JevCLIConfig(config.AIJev{APIKey: "jev-inline"})
	runner, err := buildAI(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err != nil {
		t.Fatalf("buildAI with jev api_key from config: %v", err)
	}
	if runner == nil {
		t.Fatal("buildAI returned nil runner")
	}
}

func TestT74BuildAIJevAPIKeyEnvFallback(t *testing.T) {
	cfg := t74JevCLIConfig(config.AIJev{APIKeyEnv: "T74_JEV_KEY"})
	t.Setenv("T74_JEV_KEY", "jev-env-key")
	if _, err := buildAI(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false); err != nil {
		t.Fatalf("buildAI with jev api_key_env: %v", err)
	}
}

func TestT74BuildAIJevNoKeyFailsFast(t *testing.T) {
	cfg := t74JevCLIConfig(config.AIJev{})
	if _, err := buildAI(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false); err == nil {
		t.Fatal("buildAI with neither jev api_key nor api_key_env must fail fast")
	}
}

func TestT74BuildAIJevEnvUnsetFailsFast(t *testing.T) {
	cfg := t74JevCLIConfig(config.AIJev{APIKeyEnv: "T74_JEV_KEY"})
	t.Setenv("T74_JEV_KEY", "")
	if _, err := buildAI(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false); err == nil {
		t.Fatal("buildAI with an unset api_key_env must fail fast")
	}
}
