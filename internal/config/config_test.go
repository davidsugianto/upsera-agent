package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
llm:
  provider: anthropic
  planner_model: planner-x
  executor_model: executor-y
  api_key_env: ANTHROPIC_API_KEY
datadog:
  site: datadoghq.com
clusters:
  - name: kind-upsera-lab
    kubeconfig_context: kind-upsera-lab
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 15*time.Second {
		t.Errorf("read_timeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Agent.MaxToolCalls != 15 {
		t.Errorf("max_tool_calls = %d", cfg.Agent.MaxToolCalls)
	}
	if cfg.Agent.MaxCostUSD != 0.25 {
		t.Errorf("max_cost_usd = %v", cfg.Agent.MaxCostUSD)
	}
	if cfg.Agent.MaxDuration != 120*time.Second {
		t.Errorf("max_duration = %v", cfg.Agent.MaxDuration)
	}
	if cfg.Datadog.APIKeyEnv != "DD_API_KEY" || cfg.Datadog.AppKeyEnv != "DD_APP_KEY" {
		t.Errorf("datadog env defaults = %q %q", cfg.Datadog.APIKeyEnv, cfg.Datadog.AppKeyEnv)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	body := validYAML + `
server:
  addr: ":9090"
  read_timeout: 3s
agent:
  max_tool_calls: 5
  max_cost_usd: 1.5
  max_duration: 10s
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" || cfg.Server.ReadTimeout != 3*time.Second {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Agent.MaxToolCalls != 5 || cfg.Agent.MaxCostUSD != 1.5 || cfg.Agent.MaxDuration != 10*time.Second {
		t.Errorf("agent = %+v", cfg.Agent)
	}
}

func TestLoadParsesClusters(t *testing.T) {
	body := validYAML + `
`
	body = strings.Replace(body, "    kubeconfig_context: kind-upsera-lab\n", `    kubeconfig_context: kind-upsera-lab
    namespaces:
      allow: [app]
      deny: [kube-system, kube-node-lease]
`, 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Clusters[0]
	if c.Name != "kind-upsera-lab" || c.KubeconfigContext != "kind-upsera-lab" {
		t.Errorf("cluster = %+v", c)
	}
	if len(c.Namespaces.Allow) != 1 || c.Namespaces.Allow[0] != "app" || len(c.Namespaces.Deny) != 2 {
		t.Errorf("namespaces = %+v", c.Namespaces)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeConfig(t, validYAML+"observability:\n  prometheus:\n    url: x\n"))
	if err == nil || !strings.Contains(err.Error(), "observability") {
		t.Fatalf("err = %v, want unknown key error naming observability", err)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "server: [")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadEmptyFileReportsValidationErrors(t *testing.T) {
	_, err := Load(writeConfig(t, ""))
	if err == nil || !strings.Contains(err.Error(), "llm.provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
		want   []string
	}{
		{"bad provider", func(c *Config) { c.LLM.Provider = "bedrock" }, []string{"llm.provider", "bedrock"}},
		{"empty provider", func(c *Config) { c.LLM.Provider = "" }, []string{"llm.provider"}},
		{"empty planner", func(c *Config) { c.LLM.PlannerModel = "" }, []string{"llm.planner_model"}},
		{"empty executor", func(c *Config) { c.LLM.ExecutorModel = "" }, []string{"llm.executor_model"}},
		{"empty site", func(c *Config) { c.Datadog.Site = "" }, []string{"datadog.site"}},
		{"compat needs base_url", func(c *Config) { c.LLM.Provider = "openai-compatible" }, []string{"llm.base_url"}},
		{"empty cluster name", func(c *Config) { c.Clusters[0].Name = "" }, []string{"clusters[0].name"}},
		{"duplicate cluster", func(c *Config) {
			c.Clusters = append(c.Clusters, ClusterConfig{Name: c.Clusters[0].Name})
		}, []string{"clusters[1].name", "duplicate"}},
		{"empty addr", func(c *Config) { c.Server.Addr = "" }, []string{"server.addr"}},
		{"zero read_timeout", func(c *Config) { c.Server.ReadTimeout = 0 }, []string{"server.read_timeout"}},
		{"negative read_timeout", func(c *Config) { c.Server.ReadTimeout = -time.Second }, []string{"server.read_timeout"}},
		{"zero max_tool_calls", func(c *Config) { c.Agent.MaxToolCalls = 0 }, []string{"agent.max_tool_calls"}},
		{"negative max_tool_calls", func(c *Config) { c.Agent.MaxToolCalls = -1 }, []string{"agent.max_tool_calls"}},
		{"zero max_cost_usd", func(c *Config) { c.Agent.MaxCostUSD = 0 }, []string{"agent.max_cost_usd"}},
		{"negative max_cost_usd", func(c *Config) { c.Agent.MaxCostUSD = -0.5 }, []string{"agent.max_cost_usd"}},
		{"NaN max_cost_usd", func(c *Config) { c.Agent.MaxCostUSD = math.NaN() }, []string{"agent.max_cost_usd"}},
		{"infinite max_cost_usd", func(c *Config) { c.Agent.MaxCostUSD = math.Inf(1) }, []string{"agent.max_cost_usd"}},
		{"zero max_duration", func(c *Config) { c.Agent.MaxDuration = 0 }, []string{"agent.max_duration"}},
		{"negative max_duration", func(c *Config) { c.Agent.MaxDuration = -time.Second }, []string{"agent.max_duration"}},
		{"aggregates", func(c *Config) {
			c.LLM.Provider = "nope"
			c.LLM.PlannerModel = ""
			c.Datadog.Site = ""
		}, []string{"llm.provider", "llm.planner_model", "datadog.site"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, validYAML))
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(cfg)
			err = cfg.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

func TestValidateAllowedProviders(t *testing.T) {
	for _, p := range []string{"anthropic", "openai", "gemini"} {
		cfg, err := Load(writeConfig(t, validYAML))
		if err != nil {
			t.Fatal(err)
		}
		cfg.LLM.Provider = p
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	cfg, _ := Load(writeConfig(t, validYAML))
	cfg.LLM.Provider = "openai-compatible"
	cfg.LLM.BaseURL = "http://localhost:11434/v1"
	if err := cfg.Validate(); err != nil {
		t.Errorf("openai-compatible with base_url: %v", err)
	}
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("TEST_SECRET_VAR", "s3cr3t-value")
	got, err := ResolveEnv("TEST_SECRET_VAR")
	if err != nil || got != "s3cr3t-value" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestResolveEnvMissingNamesVariableNotValue(t *testing.T) {
	_, err := ResolveEnv("DEFINITELY_UNSET_VAR_XYZ")
	if err == nil || !strings.Contains(err.Error(), "DEFINITELY_UNSET_VAR_XYZ") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveEnvEmptyName(t *testing.T) {
	if _, err := ResolveEnv(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveEnvEmptyValueIsError(t *testing.T) {
	t.Setenv("TEST_EMPTY_VAR", "")
	if _, err := ResolveEnv("TEST_EMPTY_VAR"); err == nil || !strings.Contains(err.Error(), "TEST_EMPTY_VAR") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadDoesNotRequireKeysToBeSet(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := Load(writeConfig(t, validYAML)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "config.example.yaml"))
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if len(cfg.Clusters) == 0 {
		t.Error("example config has no clusters")
	}
}

// An explicit zero in the file must not silently disable a hard stop or the
// HTTP read timeout.
func TestLoadRejectsExplicitZeroBudgets(t *testing.T) {
	body := validYAML + `
server:
  read_timeout: 0s
agent:
  max_tool_calls: 0
  max_cost_usd: 0
  max_duration: 0s
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected error for explicit zero values")
	}
	for _, w := range []string{"server.read_timeout", "agent.max_tool_calls", "agent.max_cost_usd", "agent.max_duration"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q missing %q", err, w)
		}
	}
}

// yaml.v3 decodes .nan and .inf into a float64, and NaN compares false with
// everything, so a plain "<= 0" check would let them through.
func TestLoadRejectsNaNAndInfBudget(t *testing.T) {
	for _, v := range []string{".nan", ".inf"} {
		t.Run(v, func(t *testing.T) {
			_, err := Load(writeConfig(t, validYAML+"agent:\n  max_cost_usd: "+v+"\n"))
			if err == nil || !strings.Contains(err.Error(), "agent.max_cost_usd") {
				t.Fatalf("expected max_cost_usd error for %s, got %v", v, err)
			}
		})
	}
}
