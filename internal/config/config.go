// Package config loads and validates the agent's YAML configuration.
//
// Secrets are never stored in the file. Fields named *_env hold the name of an
// environment variable, which is resolved with ResolveEnv only at the moment
// the secret is used.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Supported values for LLMConfig.Provider.
const (
	ProviderAnthropic        = "anthropic"
	ProviderOpenAI           = "openai"
	ProviderGemini           = "gemini"
	ProviderOpenAICompatible = "openai-compatible"
)

// Config is the root of the agent configuration file.
type Config struct {
	Server   ServerConfig    `yaml:"server"`
	Agent    AgentConfig     `yaml:"agent"`
	LLM      LLMConfig       `yaml:"llm"`
	Datadog  DatadogConfig   `yaml:"datadog"`
	Clusters []ClusterConfig `yaml:"clusters"`
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	Addr        string        `yaml:"addr"`
	ReadTimeout time.Duration `yaml:"read_timeout"`
}

// AgentConfig holds the hard budgets for one investigation.
type AgentConfig struct {
	MaxToolCalls int           `yaml:"max_tool_calls"`
	MaxCostUSD   float64       `yaml:"max_cost_usd"`
	MaxDuration  time.Duration `yaml:"max_duration"`
}

// LLMConfig selects the provider and the model for each role.
type LLMConfig struct {
	Provider      string `yaml:"provider"`
	PlannerModel  string `yaml:"planner_model"`
	ExecutorModel string `yaml:"executor_model"`
	APIKeyEnv     string `yaml:"api_key_env"`
	BaseURL       string `yaml:"base_url"`
}

// DatadogConfig points at the Datadog site and the env vars holding its keys.
type DatadogConfig struct {
	Site      string `yaml:"site"`
	APIKeyEnv string `yaml:"api_key_env"`
	AppKeyEnv string `yaml:"app_key_env"`
}

// ClusterConfig describes one target cluster the agent may investigate.
type ClusterConfig struct {
	Name              string           `yaml:"name"`
	KubeconfigContext string           `yaml:"kubeconfig_context"`
	Namespaces        NamespaceFilters `yaml:"namespaces"`
}

// NamespaceFilters restrict which namespaces the agent may read. An empty
// Allow list means all namespaces except those in Deny.
type NamespaceFilters struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

func defaults() Config {
	return Config{
		Server: ServerConfig{Addr: ":8080", ReadTimeout: 15 * time.Second},
		Agent:  AgentConfig{MaxToolCalls: 15, MaxCostUSD: 0.25, MaxDuration: 120 * time.Second},
		Datadog: DatadogConfig{ //nolint:gosec // env var names, not credentials
			APIKeyEnv: "DD_API_KEY",
			AppKeyEnv: "DD_APP_KEY",
		},
	}
}

// Load reads, defaults, and validates the YAML file at path. Unknown keys are
// an error. Secret env vars are not resolved here.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied config location
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// An empty document yields io.EOF; defaults then fail validation clearly.
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate reports every problem found, not just the first.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	// Zero means "no limit" to net/http and is ambiguous for the budgets, which
	// are hard stops, so every one of these must be positive.
	if c.Server.Addr == "" {
		add("server.addr must not be empty")
	}
	if c.Server.ReadTimeout <= 0 {
		add("server.read_timeout must be greater than 0")
	}
	if c.Agent.MaxToolCalls <= 0 {
		add("agent.max_tool_calls must be greater than 0")
	}
	// NaN is not <= 0, so the comparison is written to fail for it too.
	if !(c.Agent.MaxCostUSD > 0) || math.IsInf(c.Agent.MaxCostUSD, 0) {
		add("agent.max_cost_usd must be greater than 0")
	}
	if c.Agent.MaxDuration <= 0 {
		add("agent.max_duration must be greater than 0")
	}

	switch c.LLM.Provider {
	case ProviderAnthropic, ProviderOpenAI, ProviderGemini:
	case ProviderOpenAICompatible:
		if c.LLM.BaseURL == "" {
			add("llm.base_url is required when llm.provider is %q", ProviderOpenAICompatible)
		}
	default:
		add("llm.provider %q is not one of: %s, %s, %s, %s", c.LLM.Provider,
			ProviderAnthropic, ProviderOpenAI, ProviderGemini, ProviderOpenAICompatible)
	}
	if c.LLM.PlannerModel == "" {
		add("llm.planner_model must not be empty")
	}
	if c.LLM.ExecutorModel == "" {
		add("llm.executor_model must not be empty")
	}
	if c.Datadog.Site == "" {
		add("datadog.site must not be empty")
	}

	seen := make(map[string]struct{}, len(c.Clusters))
	for i, cl := range c.Clusters {
		if cl.Name == "" {
			add("clusters[%d].name must not be empty", i)
			continue
		}
		if _, dup := seen[cl.Name]; dup {
			add("clusters[%d].name %q is a duplicate", i, cl.Name)
		}
		seen[cl.Name] = struct{}{}
	}
	return errors.Join(errs...)
}

// ResolveEnv returns the value of the environment variable called name. The
// error names the variable but never includes any value.
func ResolveEnv(name string) (string, error) {
	if name == "" {
		return "", errors.New("environment variable name is not configured")
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is not set or empty", name)
	}
	return v, nil
}
