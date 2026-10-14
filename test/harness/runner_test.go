package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidsugianto/upsera-agent/internal/domain"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

// fakeClock only moves when Sleep is called, so polling loops finish at once.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

// fakeDiagnoser answers poll n (0-based) with step(n).
type fakeDiagnoser struct {
	calls  int
	closed bool
	opts   []investigation.Options
	step   func(n int) (*domain.Investigation, []domain.Evidence, error)
}

func (d *fakeDiagnoser) Diagnose(_ context.Context, opts investigation.Options) (*domain.Investigation, []domain.Evidence, error) {
	n := d.calls
	d.calls++
	d.opts = append(d.opts, opts)
	return d.step(n)
}

func (d *fakeDiagnoser) Close() error { d.closed = true; return nil }

func oomResult() (*domain.Investigation, []domain.Evidence, error) {
	return inv(oomFinding()), []domain.Evidence{evidence("E2", "describe output")}, nil
}

func emptyResult() (*domain.Investigation, []domain.Evidence, error) {
	return inv(), nil, nil
}

type harnessFixture struct {
	runner *Runner
	kube   *fakeKubectl
	clock  *fakeClock
	diag   *fakeDiagnoser
	log    *bytes.Buffer
}

func newFixture(t *testing.T, step func(n int) (*domain.Investigation, []domain.Evidence, error)) *harnessFixture {
	t.Helper()
	fx := &harnessFixture{kube: &fakeKubectl{}, clock: newFakeClock(), diag: &fakeDiagnoser{step: step}, log: &bytes.Buffer{}}
	fx.runner = &Runner{
		Driver:      newTestDriver(t, fx.kube),
		NewDiagnose: func(string) (Diagnoser, error) { return fx.diag, nil },
		Clock:       fx.clock,
		KubeContext: "kind-upsera-lab",
		Log:         fx.log,
	}
	return fx
}

func runnerScenario() Scenario {
	sc := oomScenario()
	sc.Dir = "/scenarios/oomkilled"
	sc.Apply = []string{"manifest.yaml"}
	sc.TimeoutSeconds = 60
	return sc
}

func deleteCalls(f *fakeKubectl) int {
	n := 0
	for _, c := range f.recorded() {
		if strings.Contains(c, "delete namespace scenario-oomkilled") {
			n++
		}
	}
	return n
}

func TestPollStopsWhenExpectedDetectorsAppear(t *testing.T) {
	fx := newFixture(t, func(n int) (*domain.Investigation, []domain.Evidence, error) {
		if n < 2 {
			return emptyResult()
		}
		return oomResult()
	})
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusPass {
		t.Fatalf("result = %+v", res)
	}
	if fx.diag.calls != 3 {
		t.Errorf("polls = %d, want 3", fx.diag.calls)
	}
	if len(fx.clock.sleeps) != 2 || fx.clock.sleeps[0] != 10*time.Second {
		t.Errorf("sleeps = %v, want two 10s waits", fx.clock.sleeps)
	}
	if res.DurationMS != 20000 {
		t.Errorf("duration = %dms, want 20000 from the fake clock", res.DurationMS)
	}
	if !fx.diag.closed {
		t.Error("diagnoser not closed")
	}
	got := fx.diag.opts[0]
	if got.Kind != "deployment" || got.Name != "memory-hog" || got.Namespace != "scenario-oomkilled" || got.Cluster != "kind-upsera-lab" {
		t.Errorf("options = %+v", got)
	}
}

func TestPollTimeout(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return emptyResult() })
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusFail || res.ClassCorrect {
		t.Fatalf("result = %+v", res)
	}
	// 60s timeout at 10s intervals: polls at 0,10,...,60.
	if fx.diag.calls != 7 {
		t.Errorf("polls = %d, want 7", fx.diag.calls)
	}
	if !strings.Contains(res.Detail, "timed out after 60s") || !strings.Contains(res.Detail, "missing detectors: oomkilled") {
		t.Errorf("detail = %q", res.Detail)
	}
	if fx.clock.now.Sub(newFakeClock().now) != 60*time.Second {
		t.Errorf("clock advanced %v, want exactly the timeout", fx.clock.now.Sub(newFakeClock().now))
	}
	if deleteCalls(fx.kube) != 1 {
		t.Errorf("namespace not cleaned up: %v", fx.kube.recorded())
	}
}

func TestPollTimeoutDoesNotOvershootUnevenTimeout(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return emptyResult() })
	sc := runnerScenario()
	sc.TimeoutSeconds = 25
	fx.runner.RunScenario(context.Background(), sc)
	if fx.diag.calls != 3 { // 0, 10, 20; a poll at 30 would pass the deadline
		t.Errorf("polls = %d, want 3", fx.diag.calls)
	}
}

func TestPollNegativeScenarioRunsToTimeoutAndPasses(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return emptyResult() })
	sc := Scenario{ID: "oomkilled", Namespace: "scenario-oomkilled", Dir: "/s/oomkilled", Apply: []string{"manifest.yaml"},
		Kind: "deployment", Name: "healthy", TimeoutSeconds: 30, ExpectNoFindings: true}
	res := fx.runner.RunScenario(context.Background(), sc)
	if res.Status != statusPass {
		t.Fatalf("result = %+v", res)
	}
	if fx.diag.calls != 4 {
		t.Errorf("polls = %d, want 4: negative scenarios poll the whole window", fx.diag.calls)
	}
	if strings.Contains(res.Detail, "timed out") {
		t.Errorf("running out the clock is the expected outcome here: %q", res.Detail)
	}
}

func TestPollNegativeScenarioFailsOnLateFinding(t *testing.T) {
	fx := newFixture(t, func(n int) (*domain.Investigation, []domain.Evidence, error) {
		if n == 3 {
			return oomResult()
		}
		return emptyResult()
	})
	sc := Scenario{ID: "oomkilled", Namespace: "scenario-oomkilled", Dir: "/s/oomkilled", Apply: []string{"manifest.yaml"},
		Kind: "deployment", Name: "healthy", TimeoutSeconds: 30, ExpectNoFindings: true}
	res := fx.runner.RunScenario(context.Background(), sc)
	if res.Status != statusFail || res.ClassCorrect {
		t.Errorf("a finding in the final poll must fail the scenario: %+v", res)
	}
}

func TestPollToleratesEarlyDiagnoseErrors(t *testing.T) {
	fx := newFixture(t, func(n int) (*domain.Investigation, []domain.Evidence, error) {
		if n < 2 {
			return nil, nil, errors.New("deployments.apps \"memory-hog\" not found")
		}
		return oomResult()
	})
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusPass || fx.diag.calls != 3 {
		t.Errorf("result = %+v after %d polls", res, fx.diag.calls)
	}
}

func TestPollReportsPersistentDiagnoseError(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		return nil, nil, errors.New("workload not found")
	})
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusFail || !strings.Contains(res.Detail, "diagnose: workload not found") {
		t.Errorf("result = %+v", res)
	}
}

func TestPollKeepsLastGoodResultWhenALaterPollFails(t *testing.T) {
	fx := newFixture(t, func(n int) (*domain.Investigation, []domain.Evidence, error) {
		if n == 0 {
			return inv(), nil, nil
		}
		return nil, nil, errors.New("transient")
	})
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.ToolCalls != 7 {
		t.Errorf("the last successful result should be scored: %+v", res)
	}
}

func TestRunScenarioSkipsLabOnly(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		t.Fatal("must not diagnose a lab-only scenario")
		return nil, nil, nil
	})
	sc := runnerScenario()
	sc.LabOnly = true
	res := fx.runner.RunScenario(context.Background(), sc)
	if res.Status != statusSkipped || res.Detail != "lab only" {
		t.Errorf("result = %+v", res)
	}
	if len(fx.kube.calls) != 0 {
		t.Errorf("lab-only scenario ran commands: %v", fx.kube.recorded())
	}
}

func TestRunScenarioAppliesStepsInOrderWaitingBetween(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	fx.kube.fn = func(_ context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "get deployments") {
			return "deployment.apps/web", nil
		}
		return "", nil
	}
	sc := runnerScenario()
	sc.Apply = []string{"manifest.yaml", "fault.yaml"}
	res := fx.runner.RunScenario(context.Background(), sc)
	if res.Status != statusPass {
		t.Fatalf("result = %+v", res)
	}
	var verbs []string
	for _, c := range fx.kube.calls {
		verbs = append(verbs, strings.Join(c[4:], " "))
	}
	want := []string{
		// A leftover namespace from the previous run must be gone before apply.
		"wait --for=delete namespace/scenario-oomkilled --timeout=60s",
		"apply -n scenario-oomkilled -f /scenarios/oomkilled/manifest.yaml",
		"get deployments -n scenario-oomkilled -o name",
		"rollout status deployment.apps/web -n scenario-oomkilled --timeout=120s",
		"apply -n scenario-oomkilled -f /scenarios/oomkilled/fault.yaml",
		"delete namespace scenario-oomkilled --wait=false --ignore-not-found",
	}
	if strings.Join(verbs, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(verbs, "\n"), strings.Join(want, "\n"))
	}
}

func TestCleanupRunsAfterApplyError(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		t.Fatal("must not diagnose after a failed apply")
		return nil, nil, nil
	})
	fx.kube.fn = func(_ context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), " apply ") {
			return "", errors.New("invalid manifest")
		}
		return "", nil
	}
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusFail || !strings.Contains(res.Detail, "apply manifest.yaml: invalid manifest") {
		t.Errorf("result = %+v", res)
	}
	if deleteCalls(fx.kube) != 1 {
		t.Errorf("cleanup must run after an apply error: %v", fx.kube.recorded())
	}
}

func TestCleanupRunsAfterRolloutError(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	fx.kube.fn = func(_ context.Context, args []string) (string, error) {
		j := strings.Join(args, " ")
		if strings.Contains(j, "get deployments") {
			return "", errors.New("api down")
		}
		return "", nil
	}
	sc := runnerScenario()
	sc.Apply = []string{"manifest.yaml", "fault.yaml"}
	res := fx.runner.RunScenario(context.Background(), sc)
	if res.Status != statusFail || !strings.Contains(res.Detail, "wait for rollout") {
		t.Errorf("result = %+v", res)
	}
	if deleteCalls(fx.kube) != 1 {
		t.Errorf("cleanup missing: %v", fx.kube.recorded())
	}
}

func TestCleanupRunsAfterDiagnoserSetupError(t *testing.T) {
	fx := newFixture(t, nil)
	fx.runner.NewDiagnose = func(string) (Diagnoser, error) { return nil, errors.New("bad agent kubeconfig") }
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusFail || !strings.Contains(res.Detail, "bad agent kubeconfig") {
		t.Errorf("result = %+v", res)
	}
	if deleteCalls(fx.kube) != 1 {
		t.Errorf("cleanup missing: %v", fx.kube.recorded())
	}
}

func TestCleanupRunsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		cancel() // Ctrl-C arrives while polling
		return nil, nil, ctx.Err()
	})
	var cleanupCtxErr error
	fx.kube.fn = func(c context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "delete namespace") {
			cleanupCtxErr = c.Err()
		}
		return "", nil
	}
	res := fx.runner.RunScenario(ctx, runnerScenario())
	if res.Status != statusFail || !strings.Contains(res.Detail, "interrupted") {
		t.Errorf("result = %+v", res)
	}
	if deleteCalls(fx.kube) != 1 {
		t.Fatalf("cleanup must still run after cancellation: %v", fx.kube.recorded())
	}
	if cleanupCtxErr != nil {
		t.Errorf("cleanup ran on the cancelled context (%v), so a real kubectl would be killed", cleanupCtxErr)
	}
}

func TestCleanupRunsWhenContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	fx.kube.fn = func(c context.Context, args []string) (string, error) {
		if c.Err() != nil && !strings.Contains(strings.Join(args, " "), "delete namespace") {
			return "", c.Err() // a real kubectl would die on the cancelled context
		}
		return "", nil
	}
	fx.runner.RunScenario(ctx, runnerScenario())
	if deleteCalls(fx.kube) != 1 {
		t.Errorf("cleanup missing: %v", fx.kube.recorded())
	}
}

func TestCleanupFailureIsReported(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	fx.kube.fn = func(_ context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "delete namespace") {
			return "", errors.New("forbidden")
		}
		return "", nil
	}
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if !strings.Contains(res.Detail, "cleanup of scenario-oomkilled failed: forbidden") {
		t.Errorf("detail = %q", res.Detail)
	}
	if !strings.Contains(fx.log.String(), "cleanup of scenario-oomkilled failed") {
		t.Errorf("log = %q", fx.log.String())
	}
}

func TestRunnerNeverTouchesNonScenarioNamespace(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		t.Fatal("must not diagnose")
		return nil, nil, nil
	})
	sc := runnerScenario()
	sc.Namespace = "kube-system" // bypasses the loader on purpose
	res := fx.runner.RunScenario(context.Background(), sc)
	if res.Status != statusFail {
		t.Errorf("result = %+v", res)
	}
	if len(fx.kube.calls) != 0 {
		t.Errorf("commands ran against a non-scenario namespace: %v", fx.kube.recorded())
	}
}

func TestRunAllRunsInOrderAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		cancel()
		return nil, nil, ctx.Err()
	})
	a, b := runnerScenario(), runnerScenario()
	a.ID, b.ID = "a", "b"
	results, err := fx.runner.RunAll(ctx, []Scenario{a, b})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if len(results) != 1 || results[0].ID != "a" {
		t.Errorf("results = %+v, want only the interrupted scenario", results)
	}
}

func TestRunAllCompletes(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) { return oomResult() })
	a, b := runnerScenario(), runnerScenario()
	a.ID, b.ID = "a", "b"
	results, err := fx.runner.RunAll(context.Background(), []Scenario{a, b})
	if err != nil || len(results) != 2 || results[1].ID != "b" {
		t.Errorf("results = %+v err = %v", results, err)
	}
}

func TestRealClockSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (realClock{}).Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if err := (realClock{}).Sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("err = %v", err)
	}
}

// Applying into a namespace that is still terminating reports "unchanged" and
// then the objects vanish, so the scenario must not start until it is gone.
func TestRunScenarioDoesNotApplyWhileOldNamespaceLingers(t *testing.T) {
	fx := newFixture(t, func(int) (*domain.Investigation, []domain.Evidence, error) {
		t.Fatal("must not diagnose")
		return nil, nil, nil
	})
	fx.kube.fn = func(_ context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "wait --for=delete") {
			return "", errors.New("timed out waiting for the condition")
		}
		return "", nil
	}
	res := fx.runner.RunScenario(context.Background(), runnerScenario())
	if res.Status != statusFail || !strings.Contains(res.Detail, "previous run") {
		t.Errorf("result = %+v", res)
	}
	for _, c := range fx.kube.recorded() {
		if strings.Contains(c, " apply ") {
			t.Errorf("applied despite the lingering namespace: %v", fx.kube.recorded())
		}
	}
}
