package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/agent/llm/llmtest"
	"github.com/davidsugianto/upsera-agent/internal/config"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

// reply scripts a model answer whose single hypothesis says statement and cites ids.
func reply(statement string, ids ...string) llm.Response {
	return llmtest.JSON(map[string]any{
		"hypotheses": []map[string]any{{
			"statement": statement, "confidence": "high", "evidence_ids": ids, "disproof": "",
		}},
		"ruled_out": []any{},
		"unknowns":  []string{},
	})
}

const (
	goodStatement = "The container exceeded its memory limit and was OOMKilled."
	badStatement  = "The node ran out of disk."
)

func goodReply() llm.Response { return reply(goodStatement, "E2") }
func badReply() llm.Response  { return reply(badStatement, "E2") }

func llmFixture(t *testing.T, fake *llmtest.Fake, runs int) *harnessFixture {
	t.Helper()
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	fx.runner.LLM = &LLMMode{Client: fake, Model: "test-model", MaxTokens: 30000, Runs: runs}
	return fx
}

func hyp(rank int, statement, disproof string, ids ...string) domain.Hypothesis {
	return domain.Hypothesis{Rank: rank, Statement: statement, Confidence: domain.ConfidenceHigh, EvidenceIDs: ids, Disproof: disproof}
}

func TestScoreRun(t *testing.T) {
	stored := []domain.Evidence{evidence("E2", "describe output")}
	tests := []struct {
		name          string
		classOK       bool
		sum           investigation.Summary
		wantPass      bool
		wantCause     bool
		wantIntegrity bool
		wantDetail    string
	}{
		{
			name: "cause term in the top hypothesis", classOK: true,
			sum:      investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "Hit its Memory Limit", "", "E2")}},
			wantPass: true, wantCause: true, wantIntegrity: true,
		},
		{
			name: "cause term only in the disproof counts", classOK: true,
			sum:      investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "OOMKilled", "no memory limit set", "E2")}},
			wantPass: true, wantCause: true, wantIntegrity: true,
		},
		{
			name: "cause term only in rank 2", classOK: true,
			sum: investigation.Summary{Hypotheses: []domain.Hypothesis{
				hyp(1, "disk pressure", "", "E2"), hyp(2, "memory limit too low", "", "E2"),
			}},
			wantIntegrity: true, wantDetail: "rank-1",
		},
		{
			name: "rank not slice order picks the top", classOK: true,
			sum: investigation.Summary{Hypotheses: []domain.Hypothesis{
				hyp(2, "disk pressure", "", "E2"), hyp(1, "memory limit too low", "", "E2"),
			}},
			wantPass: true, wantCause: true, wantIntegrity: true,
		},
		{
			name: "degraded run", classOK: true,
			sum:           investigation.Summary{Degraded: "model declined (no category)"},
			wantIntegrity: true, wantDetail: "degraded",
		},
		{
			name: "non-stored evidence id", classOK: true,
			sum:        investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E2", "E9")}},
			wantCause:  true,
			wantDetail: "E9",
		},
		{
			name: "hypothesis citing nothing", classOK: true,
			sum:        investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "")}},
			wantCause:  true,
			wantDetail: "cites no evidence",
		},
		{
			name: "ruled-out claim citing a non-stored id", classOK: true,
			sum: investigation.Summary{
				Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E2")},
				RuledOut:   []domain.RuledOut{{Statement: "disk", Reason: "ok", EvidenceIDs: []string{"E7"}}},
			},
			wantCause: true, wantDetail: "E7",
		},
		{
			name: "wrong class makes the cause wrong", classOK: false,
			sum:           investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E2")}},
			wantIntegrity: true, wantDetail: "class",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreRun(oomScenario(), tt.classOK, &tt.sum, stored)
			if got.Pass != tt.wantPass || got.CauseCorrect != tt.wantCause || got.EvidenceIntegrity != tt.wantIntegrity {
				t.Errorf("pass=%v cause=%v integrity=%v, want %v %v %v (detail %q)",
					got.Pass, got.CauseCorrect, got.EvidenceIntegrity, tt.wantPass, tt.wantCause, tt.wantIntegrity, got.Detail)
			}
			if tt.wantDetail != "" && !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tt.wantDetail)
			}
			if got.Hypotheses == nil || got.Rejected == nil {
				t.Error("hypotheses and rejected_claims must encode as lists, not null")
			}
		})
	}
}

func TestScoreRunCountsClaimsAndTokens(t *testing.T) {
	sum := investigation.Summary{
		Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E2")},
		RuledOut:   []domain.RuledOut{{Statement: "disk", EvidenceIDs: []string{"E2"}}},
		Rejected:   []string{"hypothesis 2: no valid evidence ID"},
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 40},
	}
	got := scoreRun(oomScenario(), true, &sum, []domain.Evidence{evidence("E2", "x")})
	if got.Claims != 3 || got.TokensIn != 100 || got.TokensOut != 40 || len(got.Rejected) != 1 {
		t.Errorf("run = %+v", got)
	}
}

func TestRunsNeeded(t *testing.T) {
	for n, want := range map[int]int{1: 1, 2: 2, 3: 2, 4: 2, 5: 2} {
		if got := runsNeeded(n); got != want {
			t.Errorf("runsNeeded(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestRunnerLLMMajorityRule(t *testing.T) {
	tests := []struct {
		name       string
		runs       int
		replies    []llm.Response
		wantPassed int
		wantStatus string
	}{
		{"3 of 3", 3, []llm.Response{goodReply(), goodReply(), goodReply()}, 3, statusPass},
		{"2 of 3", 3, []llm.Response{goodReply(), badReply(), goodReply()}, 2, statusPass},
		{"1 of 3", 3, []llm.Response{badReply(), goodReply(), badReply()}, 1, statusFail},
		{"1 of 2 fails (all must pass below 3)", 2, []llm.Response{goodReply(), badReply()}, 1, statusFail},
		{"2 of 2", 2, []llm.Response{goodReply(), goodReply()}, 2, statusPass},
		{"1 of 1", 1, []llm.Response{goodReply()}, 1, statusPass},
		{"0 of 1", 1, []llm.Response{badReply()}, 0, statusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &llmtest.Fake{Responses: tt.replies}
			fx := llmFixture(t, fake, tt.runs)
			res := fx.runner.RunScenario(context.Background(), runnerScenario())
			if res.LLMResult == nil || res.RunsPassed != tt.wantPassed || len(res.Runs) != tt.runs || res.Status != tt.wantStatus {
				t.Fatalf("result = %+v", res)
			}
			if res.Status == statusPass && res.Detail != "" {
				t.Errorf("detail on a pass = %q", res.Detail)
			}
			if res.Status == statusFail && !strings.Contains(res.Detail, "runs passed") {
				t.Errorf("detail = %q", res.Detail)
			}
		})
	}
}

func TestRunnerLLMReusesOneDiagnoseResult(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{goodReply(), goodReply(), goodReply()}}
	fx := llmFixture(t, fake, 3)

	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusPass {
		t.Fatalf("result = %+v", res)
	}
	if fx.diag.calls != 1 {
		t.Errorf("Diagnose called %d times, want once (only model variance is measured)", fx.diag.calls)
	}
	if len(fake.Requests) != 3 {
		t.Fatalf("model calls = %d, want 3", len(fake.Requests))
	}
	for _, req := range fake.Requests {
		if req.Model != "test-model" {
			t.Errorf("request model = %q", req.Model)
		}
		// The prompt carries a fresh nonce per call, so compare the content.
		if !strings.Contains(req.Messages[0].Content, "describe output") || !strings.Contains(req.Messages[0].Content, "OOMKilled") {
			t.Error("every run must be given the same findings and evidence")
		}
	}
}

// A single Budgeted shared by the runs would refuse the second call, so three
// successful runs show each run got its own budget.
func TestRunnerLLMAppliesAFreshBudgetPerRun(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{goodReply(), goodReply(), goodReply()}}
	fx := llmFixture(t, fake, 3)
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	for i, run := range res.Runs {
		if run.Degraded != "" {
			t.Errorf("run %d degraded: %s", i+1, run.Degraded)
		}
	}
}

func TestRunnerLLMTokenBudgetIsEnforced(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{goodReply()}}
	fx := llmFixture(t, fake, 1)
	fx.runner.LLM.MaxTokens = 10 // below any prompt
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if len(fake.Requests) != 0 {
		t.Errorf("an over-budget request reached the model: %d calls", len(fake.Requests))
	}
	if res.Status != statusFail || !strings.Contains(res.Runs[0].Degraded, "budget") {
		t.Errorf("result = %+v", res)
	}
}

func TestRunnerLLMExpectNoFindingsMakesNoModelCall(t *testing.T) {
	fake := &llmtest.Fake{}
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return emptyResult() })
	fx.runner.LLM = &LLMMode{Client: fake, Model: "m", MaxTokens: 30000, Runs: 3}
	sc := runnerScenario()
	sc.ExpectNoFindings, sc.ExpectedDetectors, sc.CauseTerms = true, nil, nil
	sc.TimeoutSeconds = 20

	res := fx.runner.RunScenario(context.Background(), sc)
	if len(fake.Requests) != 0 {
		t.Errorf("model called %d times for a negative scenario", len(fake.Requests))
	}
	if res.Status != statusPass || !strings.Contains(res.Detail, "model was not called") {
		t.Errorf("result = %+v", res)
	}
	if res.LLMResult == nil || len(res.Runs) != 0 {
		t.Errorf("runs = %+v", res.LLMResult)
	}
}

func TestRunnerLLMNoModelCallWithoutFindings(t *testing.T) {
	fake := &llmtest.Fake{}
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return emptyResult() })
	fx.runner.LLM = &LLMMode{Client: fake, Model: "m", MaxTokens: 30000, Runs: 3}
	sc := runnerScenario()
	sc.TimeoutSeconds = 20

	res := fx.runner.RunScenario(context.Background(), sc)
	if len(fake.Requests) != 0 || res.Status != statusFail {
		t.Errorf("calls=%d result=%+v", len(fake.Requests), res)
	}
}

func TestRunnerLLMCallErrorIsDataWithAnError(t *testing.T) {
	fake := &llmtest.Fake{
		Responses: make([]llm.Response, 3),
		Errs:      []error{errors.New("429 rate limited"), nil, errors.New("429 rate limited")},
	}
	fake.Responses[1] = goodReply()
	fx := llmFixture(t, fake, 3)

	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusFail || res.RunsPassed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if res.Runs[0].Error == "" || res.Runs[1].Error != "" {
		t.Errorf("errors = %q / %q", res.Runs[0].Error, res.Runs[1].Error)
	}
}

func TestRunnerLLMInterruptedReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &llmtest.Fake{Responses: []llm.Response{goodReply()}}
	fx := llmFixture(t, fake, 3)
	fx.runner.LLM.Client = cancellingClient{cancel: cancel, inner: fake}

	res := fx.runner.RunScenario(ctx, runnerScenario())
	if res.Status != statusFail || !strings.Contains(res.Detail, "interrupted") {
		t.Errorf("result = %+v", res)
	}
	if deleteCalls(fx.kube) != 1 {
		t.Errorf("namespace not cleaned up: %v", fx.kube.recorded())
	}
}

type cancellingClient struct {
	cancel context.CancelFunc
	inner  llm.Client
}

func (c cancellingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.cancel()
	return c.inner.Complete(ctx, req)
}

func TestBuildLLMReport(t *testing.T) {
	results := []Result{
		{ID: "a", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true,
			LLMResult: &LLMResult{Runs: []RunResult{{Pass: true}}, RunsPassed: 1, Claims: 4, RejectedClaims: 1, LLMTokensIn: 100, LLMTokensOut: 20}},
		{ID: "b", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true,
			LLMResult: &LLMResult{Runs: []RunResult{}}}, // negative scenario: no model call
		{ID: "lab", Status: statusSkipped},
	}
	rep := BuildLLMReport(results, reportTime, "test-model", 3)
	s := rep.Summary
	if s.Mode != "llm" || s.LLMSummary == nil || s.Model != "test-model" || s.Runs != 3 {
		t.Fatalf("summary = %+v", s)
	}
	if s.RejectedClaimRate != 0.25 || s.LLMTokensIn != 100 || s.LLMTokensOut != 20 {
		t.Errorf("llm summary = %+v", s.LLMSummary)
	}
	if s.Top1 != 1 || s.ClassAccuracy != 1 || s.EvidenceIntegrityRate != 1 {
		t.Errorf("rates = %+v", s)
	}
}

func TestLLMReportJSONRoundTripAndDeterministicShape(t *testing.T) {
	llmRes := Result{ID: "a", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true,
		Findings: []domain.Finding{}, Unknowns: []string{},
		LLMResult: &LLMResult{
			Runs:       []RunResult{{Pass: true, CauseCorrect: true, EvidenceIntegrity: true, Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E2")}, Rejected: []string{"x"}, Claims: 2, TokensIn: 5, TokensOut: 6, DurationMS: 7}},
			RunsPassed: 1, Claims: 2, RejectedClaims: 1, LLMTokensIn: 5, LLMTokensOut: 6,
		}}
	rep := BuildLLMReport([]Result{llmRes}, reportTime, "m", 1)
	dir := t.TempDir()
	path, err := WriteReport(dir, rep)
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	var generic struct {
		Summary map[string]any   `json:"summary"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runs", "runs_passed", "rejected_claims", "claims", "llm_tokens_in", "llm_tokens_out", "class_correct", "tool_calls"} {
		if _, ok := generic.Results[0][k]; !ok {
			t.Errorf("result lacks %q", k)
		}
	}
	for _, k := range []string{"model", "runs", "rejected_claim_rate", "llm_tokens_in", "llm_tokens_out", "top1", "class_accuracy", "evidence_integrity_rate"} {
		if _, ok := generic.Summary[k]; !ok {
			t.Errorf("summary lacks %q", k)
		}
	}
	if generic.Summary["mode"] != "llm" {
		t.Errorf("mode = %v", generic.Summary["mode"])
	}

	back, err := ReadReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.Results[0].LLMResult == nil || back.Results[0].RunsPassed != 1 || back.Results[0].Runs[0].DurationMS != 7 ||
		back.Summary.LLMSummary == nil || back.Summary.Model != "m" {
		t.Errorf("round trip lost data: %+v", back)
	}
	if err := CheckReport(back, 0.8); err != nil {
		t.Errorf("--check on an llm report: %v", err)
	}

	// Deterministic reports must keep their exact shape.
	detRaw, _ := json.Marshal(BuildReport([]Result{row("a", statusPass, true, true, true)}, reportTime))
	for _, k := range []string{`"runs"`, `"model"`, `"llm_tokens_in"`, `"rejected_claim_rate"`} {
		if strings.Contains(string(detRaw), k) {
			t.Errorf("deterministic report gained %s", k)
		}
	}
}

func TestCheckReportOnLLMReportGatesIntegrity(t *testing.T) {
	bad := Result{ID: "a", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: false, LLMResult: &LLMResult{}}
	err := CheckReport(BuildLLMReport([]Result{bad}, reportTime, "m", 3), 0.8)
	if err == nil || !strings.Contains(err.Error(), "lacks evidence integrity") {
		t.Errorf("err = %v", err)
	}
}

func TestPrintBaseline(t *testing.T) {
	base := BuildReport([]Result{
		row("oomkilled", statusPass, true, true, true),
		row("badconfig", statusFail, true, false, true),
		{ID: "lab", Status: statusSkipped},
	}, reportTime)
	rep := BuildLLMReport([]Result{
		{ID: "oomkilled", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true,
			LLMResult: &LLMResult{Runs: make([]RunResult, 3), RunsPassed: 3, LLMTokensIn: 900, LLMTokensOut: 120}},
		{ID: "badconfig", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true,
			LLMResult: &LLMResult{Runs: make([]RunResult, 3), RunsPassed: 2, LLMTokensIn: 800, LLMTokensOut: 100}},
		{ID: "healthy", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true,
			LLMResult: &LLMResult{Runs: []RunResult{}}},
	}, reportTime, "m", 3)

	var out strings.Builder
	PrintBaseline(&out, rep, base)
	got := out.String()
	for _, want := range []string{"oomkilled  yes", "3/3", "900/120", "badconfig  NO", "2/3", "healthy", "n/a", "deterministic top1 0.50 -> llm top1 1.00"} {
		if !strings.Contains(got, want) {
			t.Errorf("baseline table lacks %q:\n%s", want, got)
		}
	}
}

// ---- main wiring ----

const llmConfigYAML = `server:
  addr: ":8080"
  read_timeout: 15s
agent:
  max_tool_calls: 25
  max_llm_tokens: 30000
  max_cost_usd: 0.25
  max_duration: 120s
llm:
  provider: anthropic
  planner_model: planner-x
  executor_model: executor-x
  api_key_env: TEST_KEY_ENV
datadog:
  site: datadoghq.com
`

func writeConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(llmConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func llmDeps(k *fakeKubectl, client llm.Client) deps {
	d := testDeps(k, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	d.getenv = func(string) string { return "secret-value" }
	d.newLLM = func(config.LLMConfig, func(string) string) (llm.Client, error) { return client, nil }
	return d
}

func TestLLMFlagConflictsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--llm", "--check"},
		{"--llm", "--validate"},
		{"--llm", "--runs", "0"},
		{"--llm", "--runs", "-1"},
	} {
		if code, _, _ := execute(t, testDeps(&fakeKubectl{}, nil), args...); code != 2 {
			t.Errorf("args %v: code = %d, want 2", args, code)
		}
	}
	// --runs is ignored without --llm.
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	if code, _, _ := execute(t, testDeps(&fakeKubectl{}, nil), "--validate", "--scenarios", root, "--runs", "0"); code != 0 {
		t.Errorf("--runs must be ignored without --llm: code = %d", code)
	}
}

func TestLLMMissingKeyExitsBeforeAnyClusterCommand(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	k := &fakeKubectl{}
	d := testDeps(k, nil)
	d.getenv = func(string) string { return "" }
	d.newLLM = llm.New
	verified := false
	d.verifyServer = func(string, string) error { verified = true; return nil }

	code, _, stderr := execute(t, d, evalArgs(root, t.TempDir(), "--llm", "--config", writeConfig(t))...)
	if code != 1 || !strings.Contains(stderr, "TEST_KEY_ENV") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(k.recorded()) != 0 || verified {
		t.Errorf("cluster was touched before the key check: kubectl=%v verified=%v", k.recorded(), verified)
	}
}

func TestLLMBadConfigAndBaselineExitBeforeAnyClusterCommand(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	for name, extra := range map[string][]string{
		"missing config":   {"--llm", "--config", filepath.Join(t.TempDir(), "nope.yaml")},
		"missing baseline": {"--llm", "--config", writeConfig(t), "--baseline", filepath.Join(t.TempDir(), "nope.json")},
	} {
		t.Run(name, func(t *testing.T) {
			k := &fakeKubectl{}
			code, _, stderr := execute(t, llmDeps(k, &llmtest.Fake{}), evalArgs(root, t.TempDir(), extra...)...)
			if code != 1 || stderr == "" {
				t.Errorf("code=%d stderr=%q", code, stderr)
			}
			if len(k.recorded()) != 0 {
				t.Errorf("commands ran: %v", k.recorded())
			}
		})
	}
}

func TestLLMEvalEndToEnd(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	report := filepath.Join(t.TempDir(), "llm")

	baseDir := t.TempDir()
	if _, err := WriteReport(baseDir, BuildReport([]Result{row("oomkilled", statusPass, true, true, true)}, reportTime)); err != nil {
		t.Fatal(err)
	}

	var captured config.LLMConfig
	var keyEnv string
	fake := &llmtest.Fake{Responses: []llm.Response{goodReply(), goodReply(), badReply()}}
	k := &fakeKubectl{}
	d := llmDeps(k, fake)
	d.newLLM = func(cfg config.LLMConfig, getenv func(string) string) (llm.Client, error) {
		captured, keyEnv = cfg, getenv(cfg.APIKeyEnv)
		return fake, nil
	}

	code, stdout, stderr := execute(t, d, evalArgs(root, report, "--llm", "--runs", "3",
		"--config", writeConfig(t), "--baseline", filepath.Join(baseDir, "report.json"))...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if captured.PlannerModel != "planner-x" || keyEnv != "secret-value" {
		t.Errorf("config not used: %+v key=%q", captured, keyEnv)
	}
	for _, want := range []string{"2/3", "model planner-x", "SCENARIO", "deterministic top1 1.00 -> llm top1 1.00", "report: "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout+stderr, "secret-value") {
		t.Error("the API key leaked into the output")
	}
	for _, req := range fake.Requests {
		if req.Model != "planner-x" || req.MaxTokens == 0 {
			t.Errorf("request = model %q max_tokens %d", req.Model, req.MaxTokens)
		}
	}

	rep, err := ReadReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Mode != "llm" || rep.Summary.Runs != 3 || rep.Results[0].RunsPassed != 2 || rep.Results[0].Status != statusPass {
		t.Errorf("report = %+v", rep)
	}

	// --check keeps working on an llm report, without a cluster.
	k2 := &fakeKubectl{}
	if code, out, _ := execute(t, testDeps(k2, nil), "--check", "--report", report); code != 0 || !strings.Contains(out, "check passed") {
		t.Errorf("--check on the llm report: code=%d out=%q", code, out)
	}
	if len(k2.calls) != 0 {
		t.Errorf("--check needed a cluster: %v", k2.recorded())
	}
}

func TestLLMFailingScenarioIsDataButEveryCallFailingIsNot(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)

	// Poor answers: exit 0, report written.
	report := filepath.Join(t.TempDir(), "bad")
	fake := &llmtest.Fake{Responses: []llm.Response{badReply(), badReply(), badReply()}}
	code, _, stderr := execute(t, llmDeps(&fakeKubectl{}, fake), evalArgs(root, report, "--llm", "--config", writeConfig(t))...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if rep, err := ReadReport(report); err != nil || rep.Results[0].Status != statusFail {
		t.Errorf("report = %+v err=%v", rep, err)
	}

	// Every call erroring (bad key, outage) is an infrastructure failure.
	report = filepath.Join(t.TempDir(), "down")
	errs := []error{errors.New("401"), errors.New("401"), errors.New("401")}
	down := &llmtest.Fake{Responses: make([]llm.Response, 3), Errs: errs}
	code, _, stderr = execute(t, llmDeps(&fakeKubectl{}, down), evalArgs(root, report, "--llm", "--config", writeConfig(t))...)
	if code != 1 || !strings.Contains(stderr, "every model call failed") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
	if _, err := ReadReport(report); err != nil {
		t.Errorf("the report is still written: %v", err)
	}
}

func TestLLMDefaultRunsIsThree(t *testing.T) {
	o, code, ok := parseFlags([]string{"--llm"}, &strings.Builder{})
	if !ok || code != 0 || o.runs != 3 || o.config != "deploy/dev/config.dev.yaml" || !o.llm {
		t.Errorf("options = %+v code=%d ok=%v", o, code, ok)
	}
}

// ---- required_evidence_from ----

func evidenceFromTool(id, tool string) domain.Evidence {
	e := evidence(id, "excerpt of "+id)
	e.Tool = tool
	return e
}

func TestScoreRunRequiredEvidenceFrom(t *testing.T) {
	stored := []domain.Evidence{
		evidenceFromTool("E1", "k8s_get"),
		evidenceFromTool("E2", "k8s_describe"),
		evidenceFromTool("E3", "pod_logs"),
	}
	scenario := func(tools ...string) Scenario {
		sc := oomScenario()
		sc.RequiredEvidenceFrom = tools
		return sc
	}
	tests := []struct {
		name       string
		sc         Scenario
		sum        investigation.Summary
		wantCause  bool
		wantDetail string
	}{
		{
			name:      "cites a listed tool",
			sc:        scenario("k8s_describe", "k8s_events"),
			sum:       investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E2")}},
			wantCause: true,
		},
		{
			name:      "one of several citations is listed",
			sc:        scenario("pod_logs"),
			sum:       investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E1", "E3")}},
			wantCause: true,
		},
		{
			name:       "cites only an unlisted tool",
			sc:         scenario("k8s_describe", "k8s_events"),
			sum:        investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E1", "E3")}},
			wantDetail: "rank-1 hypothesis cites no evidence from k8s_describe, k8s_events",
		},
		{
			name: "only a lower rank cites the listed tool",
			sc:   scenario("k8s_describe"),
			sum: investigation.Summary{Hypotheses: []domain.Hypothesis{
				hyp(1, "memory limit", "", "E1"), hyp(2, "memory limit", "", "E2"),
			}},
			wantDetail: "rank-1 hypothesis cites no evidence from k8s_describe",
		},
		{
			name:      "empty list means no requirement",
			sc:        scenario(),
			sum:       investigation.Summary{Hypotheses: []domain.Hypothesis{hyp(1, "memory limit", "", "E1")}},
			wantCause: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreRun(tt.sc, true, &tt.sum, stored)
			if got.CauseCorrect != tt.wantCause || got.Pass != tt.wantCause {
				t.Errorf("cause=%v pass=%v, want %v (detail %q)", got.CauseCorrect, got.Pass, tt.wantCause, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestLoadScenarioRequiredEvidenceFrom(t *testing.T) {
	withTools := validExpected + "required_evidence_from:\n  - k8s_describe\n  - pod_logs\n"
	scs, errs := LoadScenarios(writeScenario(t, t.TempDir(), "oomkilled", withTools))
	if len(errs) != 0 || len(scs) != 1 {
		t.Fatalf("scs=%d errs=%v", len(scs), errs)
	}
	if got := scs[0].RequiredEvidenceFrom; len(got) != 2 || got[0] != "k8s_describe" || got[1] != "pod_logs" {
		t.Errorf("RequiredEvidenceFrom = %v", got)
	}

	// Absent and empty both mean no requirement.
	for _, exp := range []string{validExpected, validExpected + "required_evidence_from: []\n"} {
		scs, errs := LoadScenarios(writeScenario(t, t.TempDir(), "oomkilled", exp))
		if len(errs) != 0 || len(scs[0].RequiredEvidenceFrom) != 0 {
			t.Errorf("scs=%+v errs=%v", scs, errs)
		}
	}

	_, errs = LoadScenarios(writeScenario(t, t.TempDir(), "oomkilled", validExpected+"required_evidence_from:\n  - k8s_describe\n  - kubectl_exec\n"))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `required_evidence_from: unknown tool "kubectl_exec"`) {
		t.Errorf("errs = %v", errs)
	}
}
