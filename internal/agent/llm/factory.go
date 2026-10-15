package llm

import (
	"fmt"

	anthropicconfig "github.com/anthropics/anthropic-sdk-go/config"

	"github.com/davidsugianto/upsera-agent/internal/config"
)

const openAIBaseURL = "https://api.openai.com/v1"

// New builds the Client for the configured provider. getenv resolves the API
// key variable named by cfg.APIKeyEnv; keys are never read from the config file.
func New(cfg config.LLMConfig, getenv func(string) string) (Client, error) {
	switch cfg.Provider {
	case config.ProviderAnthropic:
		return newAnthropicFromEnv(cfg, getenv)
	case config.ProviderOpenAI:
		key, err := requireKey(cfg, getenv)
		if err != nil {
			return nil, err
		}
		return NewOpenAICompat(openAIBaseURL, key, nil), nil
	case config.ProviderOpenAICompatible:
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("llm.base_url is required when llm.provider is %q", cfg.Provider)
		}
		// Local servers (Ollama, vLLM) often need no key, so a missing one is fine.
		var key string
		if cfg.APIKeyEnv != "" {
			key = getenv(cfg.APIKeyEnv)
		}
		return NewOpenAICompat(cfg.BaseURL, key, nil), nil
	case config.ProviderGemini:
		return nil, fmt.Errorf("llm.provider %q is not supported until Phase 5", cfg.Provider)
	default:
		return nil, fmt.Errorf("llm.provider %q is not supported", cfg.Provider)
	}
}

// newAnthropicFromEnv prefers the configured API key. Without one it falls back
// to the SDK's own credential chain (ANTHROPIC_AUTH_TOKEN, or a profile from
// `ant auth login`), but only after checking that such a credential exists, so
// a missing credential still fails before any cluster work.
func newAnthropicFromEnv(cfg config.LLMConfig, getenv func(string) string) (Client, error) {
	if cfg.APIKeyEnv != "" {
		if key := getenv(cfg.APIKeyEnv); key != "" {
			return NewAnthropic(key), nil
		}
	}
	if getenv("ANTHROPIC_AUTH_TOKEN") != "" || loginProfileExists() {
		return NewAnthropic(""), nil
	}
	if cfg.APIKeyEnv == "" {
		return nil, fmt.Errorf("no anthropic credential: set llm.api_key_env to the variable holding the API key, set ANTHROPIC_AUTH_TOKEN, or run `ant auth login`")
	}
	return nil, fmt.Errorf("no anthropic credential: environment variable %s (llm.api_key_env) is empty or unset; set it to the API key, set ANTHROPIC_AUTH_TOKEN, or run `ant auth login`",
		cfg.APIKeyEnv)
}

// loginProfileExists reports whether the SDK can load a profile (from
// ANTHROPIC_CONFIG_DIR, ANTHROPIC_PROFILE or the active profile). It reads
// local files only; no token is refreshed and nothing leaves the machine.
func loginProfileExists() bool {
	_, err := anthropicconfig.LoadConfig()
	return err == nil
}

// requireKey errors with the variable's name, never its value.
func requireKey(cfg config.LLMConfig, getenv func(string) string) (string, error) {
	if cfg.APIKeyEnv == "" {
		return "", fmt.Errorf("llm.api_key_env must name the environment variable holding the %s API key", cfg.Provider)
	}
	key := getenv(cfg.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("environment variable %s (llm.api_key_env) is empty or unset; it must hold the %s API key",
			cfg.APIKeyEnv, cfg.Provider)
	}
	return key, nil
}
