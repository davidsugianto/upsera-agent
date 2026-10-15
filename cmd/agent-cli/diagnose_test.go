package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/config"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

// fakeDiagnoser stands in for a real cluster. It records what it was asked.
type fakeDiagnoser struct {
	inv         *domain.Investigation
	err         error
	evidence    []domain.Evidence
	evidenceErr error

	diagnosed []investigation.Options
	closed    bool
}

func (f *fakeDiagnoser) Diagnose(_ context.Context, o investigation.Options) (*domain.Investigation, error) {
	f.diagnosed = append(f.diagnosed, o)
	return f.inv, f.err
}
func (f *fakeDiagnoser) Evidence(_ context.Context, _ string) ([]domain.Evidence, error) {
	return f.evidence, f.evidenceErr
}
func (f *fakeDiagnoser) Close() error { f.closed = true; return nil }

type factoryCall struct {
	cluster    config.ClusterConfig
	kubeconfig string
	evidenceDB string
}

// stubFactory replaces newDiagnoser for one test and returns what it saw.
func stubFactory(t *testing.T, f *fakeDiagnoser, factoryErr error) *[]factoryCall {
	t.Helper()
	var calls []factoryCall
	orig := newDiagnoser
	newDiagnoser = func(cl config.ClusterConfig, kubeconfig, evidenceDB string) (diagnoser, error) {
		calls = append(calls, factoryCall{cl, kubeconfig, evidenceDB})
		if factoryErr != nil {
			return nil, factoryErr
		}
		return f, nil
	}
	t.Cleanup(func() { newDiagnoser = orig })
	return &calls
}

const configHead = `
llm:
  provider: anthropic
  planner_model: p
  executor_model: e
datadog:
  site: datadoghq.com
clusters:
`

func writeConfig(t *testing.T, clusters string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(configHead+clusters), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const oneCluster = `
  - name: lab
    kubeconfig_context: kind-lab
    namespaces:
      deny: [kube-system]
`

const twoClusters = oneCluster + `
  - name: prod
    kubeconfig_context: gke-prod
`

func sampleInvestigation() *domain.Investigation {
	return &domain.Investigation{
		ID:      "inv-1a2b3c4d",
		Cluster: "lab",
		Findings: []domain.Finding{{
			Detector:    "oomkilled",
			Resource:    "pod/memory-hog-abc",
			Summary:     "container hog was OOM-killed",
			Quote:       "OOMKilled exitCode=137",
			EvidenceIDs: []string{"E3", "E5"},
		}},
		Unknowns:  []string{"pod_logs memory-hog-abc: previous container not found"},
		ToolCalls: 7,
	}
}

func runCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDiagnoseHelpExits0AndListsFlags(t *testing.T) {
	code, out, errOut := runCLI("diagnose", "--help")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	all := out + errOut
	for _, f := range []string{"-n", "-cluster", "-config", "-kubeconfig", "-evidence-db", "-json", "-summarize"} {
		if !strings.Contains(all, f) {
			t.Errorf("help missing %s: %q", f, all)
		}
	}
}

func TestDiagnoseUsageErrorsExit2(t *testing.T) {
	cfg := writeConfig(t, twoClusters)
	cases := map[string][]string{
		"no target":         {"diagnose", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"no slash":          {"diagnose", "memory-hog", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"empty name":        {"diagnose", "deploy/", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"empty kind":        {"diagnose", "/x", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"extra positional":  {"diagnose", "deploy/a", "deploy/b", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"missing namespace": {"diagnose", "deploy/a", "--config", cfg, "--cluster", "lab"},
		"unknown flag":      {"diagnose", "deploy/a", "-n", "demo", "--bogus"},
		"cluster ambiguous": {"diagnose", "deploy/a", "-n", "demo", "--config", cfg},
		"pod kind":          {"diagnose", "pod/a", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"node kind":         {"diagnose", "node/a", "-n", "demo", "--config", cfg, "--cluster", "lab"},
		"unsupported kind":  {"diagnose", "cronjob/a", "-n", "demo", "--config", cfg, "--cluster", "lab"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			calls := stubFactory(t, &fakeDiagnoser{}, nil)
			code, out, errOut := runCLI(args...)
			if code != 2 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if out != "" {
				t.Errorf("stdout=%q", out)
			}
			if errOut == "" {
				t.Error("expected a message on stderr")
			}
			if len(*calls) != 0 {
				t.Error("cluster dependencies must not be built on a usage error")
			}
		})
	}
}

func TestDiagnoseUnsupportedKindMessage(t *testing.T) {
	cfg := writeConfig(t, oneCluster)
	stubFactory(t, &fakeDiagnoser{}, nil)
	_, _, errOut := runCLI("diagnose", "cronjob/a", "-n", "demo", "--config", cfg)
	if !strings.Contains(errOut, "cronjob") || !strings.Contains(errOut, "deployment") {
		t.Errorf("stderr=%q", errOut)
	}
}

func TestDiagnoseConfigErrorsExit1(t *testing.T) {
	stubFactory(t, &fakeDiagnoser{}, nil)
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("not_a_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]string{
		"missing":      filepath.Join(t.TempDir(), "nope.yaml"),
		"invalid":      bad,
		"no clusters":  writeConfig(t, " []\n"),
		"unknown name": writeConfig(t, oneCluster),
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"diagnose", "deploy/a", "-n", "demo", "--config", cfg}
			if name == "unknown name" {
				args = append(args, "--cluster", "ghost")
			}
			code, _, errOut := runCLI(args...)
			if code != 1 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if errOut == "" {
				t.Error("expected a message on stderr")
			}
		})
	}
}

func TestDiagnoseDeniedNamespaceExit1BeforeAnySideEffect(t *testing.T) {
	f := &fakeDiagnoser{}
	calls := stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	db := filepath.Join(t.TempDir(), "state", "ev.db")

	code, out, errOut := runCLI("diagnose", "deploy/a", "-n", "kube-system", "--config", cfg, "--evidence-db", db)
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	if out != "" || !strings.Contains(errOut, "kube-system") {
		t.Errorf("stdout=%q stderr=%q", out, errOut)
	}
	if len(*calls) != 0 {
		t.Error("newDiagnoser must not run (no clientset, no evidence db) for a denied namespace")
	}
	if len(f.diagnosed) != 0 {
		t.Error("Diagnose must not run for a denied namespace")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("evidence db must not be created, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
		t.Errorf("evidence db directory must not be created, stat err=%v", err)
	}
}

func TestDiagnoseWiringErrorExit1(t *testing.T) {
	stubFactory(t, nil, errors.New("load kubeconfig context \"kind-lab\": boom"))
	cfg := writeConfig(t, oneCluster)
	code, _, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "boom") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestDiagnoseFatalErrorExit1(t *testing.T) {
	f := &fakeDiagnoser{err: errors.New("get deployment: not found")}
	stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	code, out, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg)
	if code != 1 || out != "" || !strings.Contains(errOut, "not found") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !f.closed {
		t.Error("diagnoser must be closed")
	}
}

func TestDiagnoseHumanOutput(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	calls := stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	db := filepath.Join(t.TempDir(), "ev.db")

	// Flags after the positional target, kubectl style.
	code, out, errOut := runCLI("diagnose", "deploy/memory-hog", "-n", "demo",
		"--config", cfg, "--kubeconfig", "/tmp/kc", "--evidence-db", db)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{
		"lab", "deployment/memory-hog", "demo", "inv-1a2b3c4d",
		"[oomkilled] pod/memory-hog-abc: container hog was OOM-killed",
		`"OOMKilled exitCode=137"`,
		"evidence: E3, E5",
		"previous container not found",
		"evidence db: " + db,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "[oomkilled]") > strings.Index(out, "previous container not found") {
		t.Errorf("findings must precede unknowns:\n%s", out)
	}
	if len(*calls) != 1 {
		t.Fatalf("factory calls=%d", len(*calls))
	}
	c := (*calls)[0]
	if c.cluster.Name != "lab" || c.cluster.KubeconfigContext != "kind-lab" || c.kubeconfig != "/tmp/kc" || c.evidenceDB != db {
		t.Errorf("factory args=%+v", c)
	}
	if len(f.diagnosed) != 1 {
		t.Fatalf("diagnosed=%d", len(f.diagnosed))
	}
	if got := f.diagnosed[0]; got.Cluster != "lab" || got.Kind != "deployment" || got.Namespace != "demo" || got.Name != "memory-hog" {
		t.Errorf("options=%+v", got)
	}
	if !f.closed {
		t.Error("diagnoser must be closed")
	}
}

func TestDiagnoseKindAliasesNormalized(t *testing.T) {
	cfg := writeConfig(t, oneCluster)
	for target, kind := range map[string]string{
		"deploy/a": "deployment", "sts/a": "statefulset", "ds/a": "daemonset", "StatefulSet/a": "statefulset",
	} {
		f := &fakeDiagnoser{inv: sampleInvestigation()}
		stubFactory(t, f, nil)
		if code, _, errOut := runCLI("diagnose", target, "-n", "demo", "--config", cfg); code != 0 {
			t.Fatalf("%s: code=%d stderr=%q", target, code, errOut)
		}
		if got := f.diagnosed[0].Kind; got != kind {
			t.Errorf("%s: kind=%q want %q", target, got, kind)
		}
	}
}

func TestDiagnoseNoFindings(t *testing.T) {
	f := &fakeDiagnoser{inv: &domain.Investigation{ID: "inv-00000000", Cluster: "lab"}}
	stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	code, out, _ := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "No known failure signatures") {
		t.Errorf("stdout=%q", out)
	}
}

func TestDiagnoseJSONOutputShape(t *testing.T) {
	f := &fakeDiagnoser{inv: &domain.Investigation{ID: "inv-00000000", Cluster: "lab"}}
	stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	code, out, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg, "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if got["investigation_id"] != "inv-00000000" || got["cluster"] != "lab" {
		t.Errorf("json=%v", got)
	}
	for _, k := range []string{"findings", "unknowns"} {
		if arr, ok := got[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s should be an empty array, got %#v", k, got[k])
		}
	}
	if !strings.Contains(out, "\n  \"") {
		t.Errorf("expected indented output:\n%s", out)
	}
	if strings.Contains(out, "evidence db:") {
		t.Error("--json must print only the investigation")
	}
}

func TestDiagnoseSingleClusterImplicit(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	if code, _, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestDiagnosePicksNamedCluster(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	calls := stubFactory(t, f, nil)
	cfg := writeConfig(t, twoClusters)
	if code, _, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg, "--cluster", "prod"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := (*calls)[0].cluster.KubeconfigContext; got != "gke-prod" {
		t.Errorf("context=%q", got)
	}
}

func TestDefaultEvidenceDB(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, want := defaultEvidenceDB(), filepath.Join("/state", "upsera-agent", "evidence.db"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, want := defaultEvidenceDB(), filepath.Join("/home/u", ".local", "state", "upsera-agent", "evidence.db"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestParseTarget(t *testing.T) {
	kind, name, err := parseTarget("sts/db")
	if err != nil || kind != "statefulset" || name != "db" {
		t.Errorf("kind=%q name=%q err=%v", kind, name, err)
	}
	if _, _, err := parseTarget("db"); err == nil {
		t.Error("expected error without slash")
	}
}

func TestDiagnosePassesToolCallCapFromConfig(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	if code, _, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := f.diagnosed[0].MaxToolCalls; got != 25 {
		t.Errorf("MaxToolCalls=%d, want the config default 25", got)
	}

	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(custom, append([]byte("agent:\n  max_tool_calls: 9\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f2, nil)
	if code, _, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", custom); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := f2.diagnosed[0].MaxToolCalls; got != 9 {
		t.Errorf("MaxToolCalls=%d, want 9", got)
	}
}

// fakeLLM is a client that must never be called: the summary step is replaced
// by the summarize seam, so the client only needs to be passed through.
type fakeLLM struct{}

func (fakeLLM) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("fakeLLM must not be called directly")
}

type llmFactoryCall struct{ cfg config.LLMConfig }

// stubLLM replaces newLLMClient and summarize for one test.
func stubLLM(t *testing.T, clientErr error, sum *investigation.Summary, sumErr error) (*[]llmFactoryCall, *summarizeCall) {
	t.Helper()
	var factoryCalls []llmFactoryCall
	got := &summarizeCall{}
	origClient, origSummarize := newLLMClient, summarize
	newLLMClient = func(cfg config.LLMConfig) (llm.Client, error) {
		factoryCalls = append(factoryCalls, llmFactoryCall{cfg})
		if clientErr != nil {
			return nil, clientErr
		}
		return fakeLLM{}, nil
	}
	summarize = func(_ context.Context, c llm.Client, inv *domain.Investigation, ev []domain.Evidence, o investigation.SummaryOptions) (*investigation.Summary, error) {
		got.calls++
		got.client, got.inv, got.evidence, got.opts = c, inv, ev, o
		return sum, sumErr
	}
	t.Cleanup(func() { newLLMClient, summarize = origClient, origSummarize })
	return &factoryCalls, got
}

type summarizeCall struct {
	calls    int
	client   llm.Client
	inv      *domain.Investigation
	evidence []domain.Evidence
	opts     investigation.SummaryOptions
}

func goodSummary() *investigation.Summary {
	return &investigation.Summary{
		Hypotheses: []domain.Hypothesis{
			{Rank: 1, Statement: "memory limit too low for the heap", Confidence: domain.ConfidenceHigh,
				EvidenceIDs: []string{"E3", "E5"}, Disproof: "heap stays flat under load"},
			{Rank: 2, Statement: "a leak in the cache", Confidence: domain.ConfidenceLow, EvidenceIDs: []string{"E5"}},
		},
		RuledOut: []domain.RuledOut{{Statement: "node pressure", Reason: "node reports no pressure", EvidenceIDs: []string{"E6"}}},
		Unknowns: []string{"no metrics for heap growth"},
		Rejected: []string{"claim without citation", "claim citing E99"},
		Usage:    llm.Usage{InputTokens: 1200, OutputTokens: 340},
		Model:    "claude-sonnet-5-5",
	}
}

func summarizeArgs(cfg string, extra ...string) []string {
	return append([]string{"diagnose", "deploy/memory-hog", "-n", "demo", "--config", cfg, "--summarize"}, extra...)
}

func TestDiagnoseSummarizeOffNeverTouchesLLM(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	factoryCalls, sc := stubLLM(t, errors.New("must not be built"), nil, nil)
	cfg := writeConfig(t, oneCluster)

	code, out, errOut := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(*factoryCalls) != 0 || sc.calls != 0 {
		t.Errorf("llm factory calls=%d summarize calls=%d, want none without --summarize", len(*factoryCalls), sc.calls)
	}
	for _, unwanted := range []string{"Hypotheses", "degraded", "tokens"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("stdout has %q without --summarize:\n%s", unwanted, out)
		}
	}
}

func TestDiagnoseSummarizeMissingKeyExit1BeforeClusterWork(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	calls := stubFactory(t, f, nil)
	factoryCalls, sc := stubLLM(t, errors.New("llm: environment variable ANTHROPIC_API_KEY is not set or empty"), nil, nil)
	cfg := writeConfig(t, oneCluster)
	db := filepath.Join(t.TempDir(), "state", "ev.db")

	code, out, errOut := runCLI(summarizeArgs(cfg, "--evidence-db", db)...)
	if code != 1 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if out != "" || !strings.Contains(errOut, "ANTHROPIC_API_KEY") {
		t.Errorf("stdout=%q stderr=%q", out, errOut)
	}
	if len(*factoryCalls) != 1 {
		t.Errorf("llm factory calls=%d", len(*factoryCalls))
	}
	if len(*calls) != 0 || len(f.diagnosed) != 0 || sc.calls != 0 {
		t.Errorf("cluster work happened: newDiagnoser=%d Diagnose=%d summarize=%d", len(*calls), len(f.diagnosed), sc.calls)
	}
	if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
		t.Errorf("evidence db directory must not be created, stat err=%v", err)
	}
}

func TestDiagnoseSummarizeSuccessHumanOutput(t *testing.T) {
	f := &fakeDiagnoser{
		inv:      sampleInvestigation(),
		evidence: []domain.Evidence{{ID: "E1"}, {ID: "E3"}},
	}
	stubFactory(t, f, nil)
	factoryCalls, sc := stubLLM(t, nil, goodSummary(), nil)
	cfg := writeConfig(t, oneCluster)

	code, out, errOut := runCLI(summarizeArgs(cfg)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{
		"Hypotheses:",
		"1. [high] memory limit too low for the heap",
		"evidence: E3, E5",
		"would be disproved by: heap stays flat under load",
		"2. [low] a leak in the cache",
		"Ruled out:",
		"node pressure",
		"node reports no pressure",
		"rejected claims: 2",
		"no metrics for heap growth",
		"claude-sonnet-5-5",
		"1200 in",
		"340 out",
		"[oomkilled] pod/memory-hog-abc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "degraded") {
		t.Errorf("a clean summary must not print degraded:\n%s", out)
	}
	if strings.Index(out, "[oomkilled]") > strings.Index(out, "Hypotheses:") {
		t.Errorf("findings must precede hypotheses:\n%s", out)
	}

	if len(*factoryCalls) != 1 {
		t.Fatalf("llm factory calls=%d", len(*factoryCalls))
	}
	if sc.calls != 1 {
		t.Fatalf("summarize calls=%d", sc.calls)
	}
	if _, ok := sc.client.(*llm.Budgeted); !ok {
		t.Errorf("summary client is %T, want *llm.Budgeted so the budget is enforced", sc.client)
	}
	if sc.opts.Model != "p" {
		t.Errorf("model=%q, want the planner model from the config", sc.opts.Model)
	}
	if len(sc.evidence) != 2 || sc.evidence[1].ID != "E3" {
		t.Errorf("evidence passed to Summarize = %+v", sc.evidence)
	}
	if sc.inv == nil || sc.inv.ID != "inv-1a2b3c4d" {
		t.Errorf("investigation passed to Summarize = %+v", sc.inv)
	}
}

func TestDiagnoseSummarizeDegradedStillExit0(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	sum := &investigation.Summary{Degraded: "model refused (category: cyber)", Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 900}}
	stubLLM(t, nil, sum, nil)
	cfg := writeConfig(t, oneCluster)

	code, out, errOut := runCLI(summarizeArgs(cfg)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "degraded: model refused (category: cyber)") {
		t.Errorf("stdout missing degraded line:\n%s", out)
	}
	if !strings.Contains(out, "[oomkilled]") {
		t.Errorf("deterministic findings must still print:\n%s", out)
	}
	if strings.Contains(out, "Hypotheses:") {
		t.Errorf("no hypotheses section when degraded:\n%s", out)
	}
	if strings.Contains(out, "rejected claims") {
		t.Errorf("rejected line is only for a positive count:\n%s", out)
	}
}

func TestDiagnoseSummarizeBudgetErrorDegrades(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	stubLLM(t, nil, nil, fmt.Errorf("summarize: %w", llm.ErrBudgetExceeded))
	cfg := writeConfig(t, oneCluster)

	code, out, errOut := runCLI(summarizeArgs(cfg)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q, a budget error must degrade, not fail", code, errOut)
	}
	if !strings.Contains(out, "degraded:") || !strings.Contains(out, "budget") {
		t.Errorf("stdout should explain the budget degradation:\n%s", out)
	}
	if !strings.Contains(out, "[oomkilled]") {
		t.Errorf("findings must still print:\n%s", out)
	}
}

func TestDiagnoseSummarizeOtherErrorDegrades(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	stubLLM(t, nil, nil, errors.New("anthropic: 529 overloaded"))
	cfg := writeConfig(t, oneCluster)

	code, out, errOut := runCLI(summarizeArgs(cfg)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "degraded:") || !strings.Contains(out, "529 overloaded") {
		t.Errorf("stdout=%s", out)
	}
}

func TestDiagnoseSummarizeEvidenceReadErrorDegrades(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation(), evidenceErr: errors.New("db locked")}
	stubFactory(t, f, nil)
	_, sc := stubLLM(t, nil, goodSummary(), nil)
	cfg := writeConfig(t, oneCluster)

	code, out, _ := runCLI(summarizeArgs(cfg)...)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if sc.calls != 0 {
		t.Error("the model must not be called without its evidence")
	}
	if !strings.Contains(out, "degraded:") || !strings.Contains(out, "db locked") {
		t.Errorf("stdout=%s", out)
	}
}

func TestDiagnoseSummarizeJSONShape(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	stubLLM(t, nil, goodSummary(), nil)
	cfg := writeConfig(t, oneCluster)

	code, out, errOut := runCLI(summarizeArgs(cfg, "--json")...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var got struct {
		Hypotheses []struct {
			Rank        int      `json:"rank"`
			Statement   string   `json:"statement"`
			Confidence  string   `json:"confidence"`
			EvidenceIDs []string `json:"evidence_ids"`
			Disproof    string   `json:"disproof"`
		} `json:"hypotheses"`
		RuledOut []struct {
			Statement   string   `json:"statement"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"ruled_out"`
		Unknowns []string `json:"unknowns"`
		LLM      *struct {
			Model        string `json:"model"`
			Calls        int    `json:"calls"`
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
		} `json:"llm"`
		RejectedClaims int    `json:"rejected_claims"`
		Degraded       string `json:"degraded"`
		ID             string `json:"investigation_id"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if got.ID != "inv-1a2b3c4d" || len(got.Hypotheses) != 2 || got.Hypotheses[0].Rank != 1 ||
		got.Hypotheses[0].Confidence != "high" || len(got.Hypotheses[0].EvidenceIDs) != 2 || got.Hypotheses[0].Disproof == "" {
		t.Errorf("hypotheses=%+v id=%q", got.Hypotheses, got.ID)
	}
	if len(got.RuledOut) != 1 || got.RuledOut[0].EvidenceIDs[0] != "E6" {
		t.Errorf("ruled_out=%+v", got.RuledOut)
	}
	if len(got.Unknowns) != 2 || got.Unknowns[1] != "no metrics for heap growth" {
		t.Errorf("model unknowns must be appended after the collection ones: %v", got.Unknowns)
	}
	if got.LLM == nil || got.LLM.Model != "claude-sonnet-5-5" || got.LLM.Calls != 1 ||
		got.LLM.InputTokens != 1200 || got.LLM.OutputTokens != 340 {
		t.Errorf("llm=%+v", got.LLM)
	}
	if got.RejectedClaims != 2 || got.Degraded != "" {
		t.Errorf("rejected_claims=%d degraded=%q", got.RejectedClaims, got.Degraded)
	}
}

func TestDiagnoseSummarizeJSONDegraded(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	stubLLM(t, nil, nil, fmt.Errorf("summarize: %w", llm.ErrBudgetExceeded))
	cfg := writeConfig(t, oneCluster)

	code, out, _ := runCLI(summarizeArgs(cfg, "--json")...)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if d, _ := got["degraded"].(string); d == "" {
		t.Errorf("degraded missing from %v", got)
	}
	for _, k := range []string{"hypotheses", "ruled_out"} {
		if arr, ok := got[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s should be an empty array, got %#v", k, got[k])
		}
	}
}

func TestDiagnoseJSONWithoutSummarizeHasNoLLMFields(t *testing.T) {
	f := &fakeDiagnoser{inv: sampleInvestigation()}
	stubFactory(t, f, nil)
	cfg := writeConfig(t, oneCluster)
	_, out, _ := runCLI("diagnose", "deploy/a", "-n", "demo", "--config", cfg, "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"llm", "degraded", "rejected_claims"} {
		if _, ok := got[k]; ok {
			t.Errorf("%q present without --summarize", k)
		}
	}
	for _, k := range []string{"hypotheses", "ruled_out"} {
		if arr, ok := got[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s should be an empty array, got %#v", k, got[k])
		}
	}
}

func TestDiagnoseSummarizeFatalDiagnoseSkipsModel(t *testing.T) {
	f := &fakeDiagnoser{err: errors.New("get deployment: not found")}
	stubFactory(t, f, nil)
	_, sc := stubLLM(t, nil, goodSummary(), nil)
	cfg := writeConfig(t, oneCluster)
	code, _, _ := runCLI(summarizeArgs(cfg)...)
	if code != 1 || sc.calls != 0 {
		t.Errorf("code=%d summarize calls=%d", code, sc.calls)
	}
}
