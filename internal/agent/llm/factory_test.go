package llm_test

import (
	"os"
	"strings"
	"testing"

	anthropicconfig "github.com/anthropics/anthropic-sdk-go/config"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/config"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestNewBuildsEachProvider(t *testing.T) {
	const secret = "sk-test-secret-value"
	tests := []struct {
		name string
		cfg  config.LLMConfig
		env  map[string]string
	}{
		{"anthropic", config.LLMConfig{Provider: config.ProviderAnthropic, APIKeyEnv: "K"}, map[string]string{"K": secret}},
		{"openai", config.LLMConfig{Provider: config.ProviderOpenAI, APIKeyEnv: "K"}, map[string]string{"K": secret}},
		{"openai-compatible with key", config.LLMConfig{Provider: config.ProviderOpenAICompatible, APIKeyEnv: "K", BaseURL: "http://localhost:11434/v1"}, map[string]string{"K": secret}},
		{"openai-compatible keyless", config.LLMConfig{Provider: config.ProviderOpenAICompatible, APIKeyEnv: "K", BaseURL: "http://localhost:11434/v1"}, nil},
		{"openai-compatible no key env", config.LLMConfig{Provider: config.ProviderOpenAICompatible, BaseURL: "http://localhost:11434/v1"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := llm.New(tt.cfg, env(tt.env))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c == nil {
				t.Fatal("New returned a nil client")
			}
		})
	}
}

// isolateAnthropicLogin points the SDK's login lookup at an empty directory so
// the developer's own `ant auth login` profile cannot change a test result.
func isolateAnthropicLogin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
	// Setenv first so the original value is restored; the SDK treats a
	// set-but-empty profile name as an error, so it must be unset.
	t.Setenv("ANTHROPIC_PROFILE", "")
	os.Unsetenv("ANTHROPIC_PROFILE")
	return dir
}

func TestNewAnthropicWithoutKeyNeedsAnotherCredential(t *testing.T) {
	isolateAnthropicLogin(t)
	cfg := config.LLMConfig{Provider: config.ProviderAnthropic, APIKeyEnv: "UPSERA_TEST_KEY"}
	_, err := llm.New(cfg, env(nil))
	if err == nil {
		t.Fatal("expected an error with no key, token or login profile")
	}
	for _, want := range []string{"UPSERA_TEST_KEY", "ANTHROPIC_AUTH_TOKEN", "ant auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err, want)
		}
	}
}

func TestNewAnthropicAcceptsAuthToken(t *testing.T) {
	isolateAnthropicLogin(t)
	cfg := config.LLMConfig{Provider: config.ProviderAnthropic, APIKeyEnv: "UPSERA_TEST_KEY"}
	c, err := llm.New(cfg, env(map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}))
	if err != nil || c == nil {
		t.Fatalf("New = %v, %v; want a client", c, err)
	}
}

func TestNewAnthropicAcceptsLoginProfile(t *testing.T) {
	dir := isolateAnthropicLogin(t)
	prof := &anthropicconfig.Config{AuthenticationInfo: anthropicconfig.NewUserOAuthAuthentication("client-id")}
	if err := anthropicconfig.SaveProfile(dir, "default", prof); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	// No api_key_env at all: a login profile alone is enough.
	c, err := llm.New(config.LLMConfig{Provider: config.ProviderAnthropic}, env(nil))
	if err != nil || c == nil {
		t.Fatalf("New = %v, %v; want a client", c, err)
	}
}

func TestNewMissingKeyNamesEnvVarOnly(t *testing.T) {
	isolateAnthropicLogin(t)
	for _, provider := range []string{config.ProviderAnthropic, config.ProviderOpenAI} {
		t.Run(provider, func(t *testing.T) {
			cfg := config.LLMConfig{Provider: provider, APIKeyEnv: "UPSERA_TEST_KEY"}
			// Other variables hold values that must never appear in the error.
			_, err := llm.New(cfg, env(map[string]string{"OTHER": "sk-do-not-leak"}))
			if err == nil {
				t.Fatal("expected an error for a missing key")
			}
			if !strings.Contains(err.Error(), "UPSERA_TEST_KEY") {
				t.Errorf("error %q does not name the env var", err)
			}
			if strings.Contains(err.Error(), "sk-do-not-leak") {
				t.Errorf("error leaks a value: %q", err)
			}
		})
	}
}

func TestNewMissingKeyEnvNameConfigured(t *testing.T) {
	isolateAnthropicLogin(t)
	_, err := llm.New(config.LLMConfig{Provider: config.ProviderAnthropic}, env(nil))
	if err == nil {
		t.Fatal("expected an error when no key env var is configured")
	}
	if !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("error %q should point at llm.api_key_env", err)
	}
}

func TestNewOpenAICompatibleRequiresBaseURL(t *testing.T) {
	_, err := llm.New(config.LLMConfig{Provider: config.ProviderOpenAICompatible}, env(nil))
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("error = %v, want one mentioning base_url", err)
	}
}

func TestNewGeminiUnsupported(t *testing.T) {
	_, err := llm.New(config.LLMConfig{Provider: config.ProviderGemini, APIKeyEnv: "K"}, env(map[string]string{"K": "x"}))
	if err == nil || !strings.Contains(err.Error(), "Phase 5") {
		t.Fatalf("error = %v, want one mentioning Phase 5", err)
	}
}

func TestNewUnknownProvider(t *testing.T) {
	_, err := llm.New(config.LLMConfig{Provider: "bogus"}, env(nil))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error = %v, want one naming the provider", err)
	}
}
