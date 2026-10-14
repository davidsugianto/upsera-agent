package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// testDeps wires fakes; created records the diagnoser handed to every scenario.
func testDeps(k *fakeKubectl, step func(n int) (*domain.Investigation, []domain.Evidence, error)) deps {
	return deps{
		kubectl: k,
		diagnosers: func(string, string, string) DiagnoserFactory {
			return func(string) (Diagnoser, error) { return &fakeDiagnoser{step: step}, nil }
		},
		clock:        newFakeClock(),
		verifyServer: func(string, string) error { return nil },
	}
}

func execute(t *testing.T, d deps, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb, d)
	return code, out.String(), errb.String()
}

func evalArgs(scenarios, report string, extra ...string) []string {
	return append([]string{
		"--scenarios", scenarios, "--report", report,
		"--admin-kubeconfig", "/tmp/admin", "--agent-kubeconfig", "/tmp/agent",
	}, extra...)
}

func TestHelpExitsZero(t *testing.T) {
	code, _, stderr := execute(t, testDeps(&fakeKubectl{}, nil), "--help")
	if code != 0 || !strings.Contains(stderr, "-min-top1") || !strings.Contains(stderr, "-admin-kubeconfig") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--nope"},
		{"stray"},
		{"--min-top1", "1.5"},
		{"--min-top1", "-0.1"},
		{"--check", "--validate"},
	} {
		code, _, _ := execute(t, testDeps(&fakeKubectl{}, nil), args...)
		if code != 2 {
			t.Errorf("args %v: code = %d, want 2", args, code)
		}
	}
}

func TestValidateMode(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	k := &fakeKubectl{}
	code, stdout, _ := execute(t, testDeps(k, nil), "--validate", "--scenarios", root)
	if code != 0 || !strings.Contains(stdout, "OK  oomkilled") || !strings.Contains(stdout, "1 scenarios valid") {
		t.Errorf("code=%d stdout=%q", code, stdout)
	}
	if len(k.calls) != 0 {
		t.Errorf("--validate must not need a cluster: %v", k.recorded())
	}

	writeScenario(t, root, "broken", "id: broken\nnamespace: prod\ntarget: deployment/x\n")
	code, _, stderr := execute(t, testDeps(k, nil), "--validate", "--scenarios", root)
	if code != 1 || !strings.Contains(stderr, "broken:") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestCheckMode(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKubectl{}
	d := testDeps(k, nil)

	code, _, stderr := execute(t, d, "--check", "--report", dir)
	if code != 1 || !strings.Contains(stderr, "read report") {
		t.Errorf("missing report: code=%d stderr=%q", code, stderr)
	}

	rep := BuildReport([]Result{
		row("a", statusPass, true, true, true), row("b", statusPass, true, true, true),
		row("c", statusPass, true, true, true), row("d", statusPass, true, true, true),
		row("e", statusFail, true, false, true),
	}, reportTime)
	if _, err := WriteReport(dir, rep); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := execute(t, d, "--check", "--report", dir, "--min-top1", "0.80"); code != 0 || !strings.Contains(stdout, "check passed") {
		t.Errorf("at threshold: code=%d stdout=%q", code, stdout)
	}
	if code, _, stderr := execute(t, d, "--check", "--report", dir, "--min-top1", "0.81"); code != 1 || !strings.Contains(stderr, "below the minimum") {
		t.Errorf("below threshold: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := execute(t, d, "--check", "--report", dir); code != 0 {
		t.Errorf("default threshold is 0.80: code=%d", code)
	}
	if len(k.calls) != 0 {
		t.Errorf("--check must not need a cluster: %v", k.recorded())
	}
}

func TestEvalRefusesNonKindContextBeforeAnything(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	report := filepath.Join(t.TempDir(), "results")
	k := &fakeKubectl{}
	called := false
	d := testDeps(k, nil)
	d.diagnosers = func(string, string, string) DiagnoserFactory {
		called = true
		return nil
	}
	code, _, stderr := execute(t, d, evalArgs(root, report, "--context", "gke_prod_eu_main")...)
	if code != 1 || !strings.Contains(stderr, "kind cluster") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
	if len(k.calls) != 0 || called {
		t.Errorf("something ran before the refusal: kubectl=%v diagnosers=%v", k.recorded(), called)
	}
	if _, err := os.Stat(report); err == nil {
		t.Error("no report should be written")
	}
}

func TestEvalDoesNotApplyWhenScenariosAreInvalid(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "good", strings.ReplaceAll(validExpected, "oomkilled", "good"))
	writeScenario(t, root, "bad", "id: bad\nnamespace: prod\ntarget: deployment/x\n")
	k := &fakeKubectl{}
	code, _, stderr := execute(t, testDeps(k, nil), evalArgs(root, t.TempDir())...)
	if code != 1 || !strings.Contains(stderr, "nothing was applied") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
	if len(k.calls) != 0 {
		t.Errorf("commands ran: %v", k.recorded())
	}
}

func TestEvalOnlyFlag(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	k := &fakeKubectl{}
	d := testDeps(k, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })

	if code, _, stderr := execute(t, d, evalArgs(root, t.TempDir(), "--only", "typo")...); code != 2 || !strings.Contains(stderr, "unknown scenario id(s): typo") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
	if len(k.calls) != 0 {
		t.Errorf("commands ran for an unknown id: %v", k.recorded())
	}
	if code, _, _ := execute(t, d, evalArgs(root, t.TempDir(), "--only", "oomkilled")...); code != 0 {
		t.Errorf("code = %d", code)
	}
}

func TestEvalWritesReportAndExitCodes(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	report := filepath.Join(t.TempDir(), "results")

	k := &fakeKubectl{}
	d := testDeps(k, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	code, stdout, stderr := execute(t, d, evalArgs(root, report)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "oomkilled") || !strings.Contains(stdout, "pass") || !strings.Contains(stdout, "report: ") {
		t.Errorf("stdout = %q", stdout)
	}
	rep, err := ReadReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Status != statusPass || rep.Summary.Top1 != 1 || rep.Summary.Mode != "deterministic" {
		t.Errorf("report = %+v", rep)
	}
	for _, c := range k.recorded() {
		if !strings.Contains(c, "--kubeconfig /tmp/admin --context kind-upsera-lab") {
			t.Errorf("command not pinned to the admin kubeconfig and kind context: %s", c)
		}
	}
	if deleteCalls(k) != 1 {
		t.Errorf("namespace not cleaned up: %v", k.recorded())
	}

	// A scenario that fails is data, not a run failure: --check is the gate, so
	// the run exits 0 and still writes the report.
	failDeps := testDeps(&fakeKubectl{}, func(int) (*domain.Investigation, []domain.Evidence, error) { return emptyResult() })
	code, _, _ = execute(t, failDeps, evalArgs(root, report, "--context", "kind-other")...)
	if code != 0 {
		t.Errorf("failing scenario: code = %d, want 0 (the gate is --check)", code)
	}
	rep, err = ReadReport(report)
	if err != nil || rep.Results[0].Status != statusFail {
		t.Errorf("report = %+v err = %v", rep, err)
	}
}

func TestEvalCancelledWritesNoReport(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	report := filepath.Join(t.TempDir(), "results")
	ctx, cancel := context.WithCancel(context.Background())
	k := &fakeKubectl{}
	d := testDeps(k, func(int) (*domain.Investigation, []domain.Evidence, error) {
		cancel()
		return nil, nil, ctx.Err()
	})
	var out, errb bytes.Buffer
	code := run(ctx, evalArgs(root, report), &out, &errb, d)
	if code != 1 || !strings.Contains(errb.String(), "interrupted") {
		t.Errorf("code=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(report, "report.json")); err == nil {
		t.Error("a partial report must not be written")
	}
	if deleteCalls(k) != 1 {
		t.Errorf("namespace not cleaned up after interrupt: %v", k.recorded())
	}
}

func TestSelectScenarios(t *testing.T) {
	all := []Scenario{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got, err := selectScenarios(all, "c, a,")
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("got %+v err %v", got, err)
	}
	if got, err := selectScenarios(all, ""); err != nil || len(got) != 3 {
		t.Errorf("got %+v err %v", got, err)
	}
	if _, err := selectScenarios(all, "a,zzz"); err == nil {
		t.Error("unknown id must error")
	}
}

func TestVerifyLoopbackServer(t *testing.T) {
	write := func(t *testing.T, server string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "kubeconfig")
		cfg := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: " + server + "\n" +
			"users:\n- name: u\n  user:\n    token: x\ncontexts:\n- name: kind-lab\n  context:\n    cluster: c\n    user: u\n"
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for server, wantErr := range map[string]bool{
		"https://127.0.0.1:6443":     false,
		"https://localhost:6443":     false,
		"https://[::1]:6443":         false,
		"https://203.0.113.5:6443":   true,
		"https://prod.example.com":   true,
		"https://127.0.0.1.evil.com": true,
	} {
		err := verifyLoopbackServer(write(t, server), "kind-lab")
		if (err != nil) != wantErr {
			t.Errorf("server %s: err = %v, wantErr %v", server, err, wantErr)
		}
	}
	if err := verifyLoopbackServer(write(t, "https://127.0.0.1:1"), "kind-missing"); err == nil {
		t.Error("an unknown context must be refused")
	}
	if err := verifyLoopbackServer(filepath.Join(t.TempDir(), "nope"), "kind-lab"); err == nil {
		t.Error("an unreadable kubeconfig must be refused")
	}
}

// The check runs before any kubectl command, so a kind-named context that
// points at a real cluster is never touched.
func TestEvalRefusesNonLoopbackServerBeforeAnyCommand(t *testing.T) {
	k := &fakeKubectl{}
	d := testDeps(k, nil)
	d.verifyServer = func(string, string) error { return errors.New("API server is not local") }
	code, _, stderr := execute(t, d, evalArgs(t.TempDir(), t.TempDir())...)
	if code != 1 || !strings.Contains(stderr, "not local") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(k.recorded()) != 0 {
		t.Fatalf("commands ran against an unverified cluster: %v", k.recorded())
	}
}
