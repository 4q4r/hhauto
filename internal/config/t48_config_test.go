package config

// T48: (1) optional [ai.providers.*].api_key — the literal API key in the
// TOML for owners who do not want environment variables. Priority is
// api_key > api_key_env (both set is valid; resolution prefers api_key).
// A provider referenced by a configured model must carry at least one of
// the two — a half-configured AI feature must not start.
// (2) [storage].cookie_store selects the cookie backend: "" or "vault"
// (default, passphrase-protected) or "plain" (0600 file, no passphrase).

import (
	"strings"
	"testing"
)

func t48AIConfig(provider AIProvider) *Config {
	c := validConfig()
	c.AI.Providers = map[string]AIProvider{"zai": provider}
	c.AI.Models = map[string]AIModel{"improve": {Provider: "zai", Model: "glm"}}
	c.AI.Tasks = map[string]AITask{"resume_improve": {ModelProfile: "improve"}}
	return c
}

func TestT48ProviderAPIKeyValidation(t *testing.T) {
	base := AIProvider{Dialect: "openai-chat", BaseURL: "https://api.example.com/v4"}

	// env-var only (the pre-T48 shape) stays valid.
	c := t48AIConfig(base)
	c.AI.Providers["zai"] = AIProvider{Dialect: base.Dialect, BaseURL: base.BaseURL, APIKeyEnv: "HHAUTO_ZAI_KEY"}
	if err := Validate(c); err != nil {
		t.Fatalf("api_key_env only must validate: %v", err)
	}

	// api_key only is valid without any environment variable — the whole
	// point of the field.
	c = t48AIConfig(base)
	c.AI.Providers["zai"] = AIProvider{Dialect: base.Dialect, BaseURL: base.BaseURL, APIKey: "secret-key"}
	if err := Validate(c); err != nil {
		t.Fatalf("api_key only must validate: %v", err)
	}

	// both set is valid (api_key wins at resolve time; no config error).
	c = t48AIConfig(base)
	c.AI.Providers["zai"] = AIProvider{Dialect: base.Dialect, BaseURL: base.BaseURL, APIKey: "secret-key", APIKeyEnv: "HHAUTO_ZAI_KEY"}
	if err := Validate(c); err != nil {
		t.Fatalf("api_key + api_key_env must validate (api_key wins): %v", err)
	}

	// neither key, but the provider is used by a model -> validation error.
	c = t48AIConfig(base)
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("used provider without any key = %v, want api_key error", err)
	}

	// neither key, provider unused -> valid (nothing consumes it).
	c = validConfig()
	c.AI.Providers = map[string]AIProvider{"idle": base}
	if err := Validate(c); err != nil {
		t.Fatalf("unused provider without keys must validate: %v", err)
	}
}

func TestT48CookieStoreValidation(t *testing.T) {
	for _, ok := range []string{"", "vault", "plain"} {
		c := validConfig()
		c.Storage.CookieStore = ok
		if err := Validate(c); err != nil {
			t.Fatalf("cookie_store %q must validate: %v", ok, err)
		}
	}
	c := validConfig()
	c.Storage.CookieStore = "keychain"
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "cookie_store") {
		t.Fatalf("cookie_store \"keychain\" = %v, want cookie_store error", err)
	}
}
