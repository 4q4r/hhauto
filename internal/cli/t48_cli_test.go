package cli

// T48 wiring: [ai.providers.*].api_key resolves without any environment
// variable; a used provider with neither api_key nor api_key_env fails
// fast at startup; [storage].cookie_store = "plain" builds the plain file
// store while the default keeps the existing vault/keyring resolution.

import (
	"io"
	"log/slog"
	"testing"

	"hhauto/internal/config"
)

func t48AIConfig(provider config.AIProvider) *config.Config {
	return &config.Config{
		Features: config.Features{AutoClone: config.AutoClone{Improve: config.AutoCloneImprove{
			Enabled: true, Fields: []string{"title"}, ModelProfile: "improve",
		}}},
		AI: config.AI{
			Providers: map[string]config.AIProvider{"zai": provider},
			Models:    map[string]config.AIModel{"improve": {Provider: "zai", Model: "glm"}},
		},
	}
}

func TestT48BuildAIAPIKeyFromConfig(t *testing.T) {
	// No HHAUTO_ZAI_KEY in the environment: the config key must be enough.
	cfg := t48AIConfig(config.AIProvider{
		Dialect: "openai-chat", BaseURL: "https://api.example.com/v4", APIKey: "t48-config-key",
	})
	runner, err := buildAI(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err != nil {
		t.Fatalf("buildAI with api_key from config: %v", err)
	}
	if runner == nil {
		t.Fatal("buildAI returned nil runner")
	}
}

func TestT48BuildAIBothKeysEmptyFails(t *testing.T) {
	cfg := t48AIConfig(config.AIProvider{Dialect: "openai-chat", BaseURL: "https://api.example.com/v4"})
	_, err := buildAI(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err == nil {
		t.Fatal("buildAI with neither api_key nor api_key_env must fail fast")
	}
}

func TestT48ResolveCookieStorePlain(t *testing.T) {
	t.Setenv("HHAUTO_COOKIES_BACKEND", "")

	cfg := &config.Config{General: config.General{DataDir: t.TempDir()}}
	cfg.Storage.CookieStore = "plain"
	s, err := resolveCookieStore(cfg)
	if err != nil {
		t.Fatalf("resolveCookieStore plain: %v", err)
	}
	if s.Backend() != "plain-file" {
		t.Fatalf("plain mode Backend() = %q, want plain-file", s.Backend())
	}

	// Default (empty) keeps the existing vault/keyring resolution.
	cfg.Storage.CookieStore = ""
	s, err = resolveCookieStore(cfg)
	if err != nil {
		t.Fatalf("resolveCookieStore default: %v", err)
	}
	if s.Backend() == "plain-file" {
		t.Fatal("default mode must not build the plain store")
	}
}
