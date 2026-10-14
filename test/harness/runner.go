package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	k8stools "github.com/davidsugianto/upsera-agent/internal/agent/tools/k8s"
	"github.com/davidsugianto/upsera-agent/internal/config"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
	"github.com/davidsugianto/upsera-agent/internal/repository/sqlite"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

const (
	pollInterval = 10 * time.Second
	// rolloutTimeout bounds the wait between apply steps. It is separate from
	// the scenario timeout, which only covers waiting for the symptom.
	rolloutTimeout = 120 * time.Second
	// cleanupTimeout bounds namespace deletion, which must still run after the
	// run's context was cancelled.
	cleanupTimeout = 30 * time.Second

	// namespaceGoneTimeout bounds the wait for a previous run's namespace to
	// finish terminating before a scenario is applied.
	namespaceGoneTimeout = 60 * time.Second
)

// Clock is injected so the polling loop can be tested without waiting.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends, whichever is first.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Diagnoser runs the deterministic diagnosis and also returns the evidence it
// stored, so the scorer can hold the findings against the store.
type Diagnoser interface {
	Diagnose(ctx context.Context, opts investigation.Options) (*domain.Investigation, []domain.Evidence, error)
	Close() error
}

// DiagnoserFactory builds a Diagnoser whose reader may only see namespace ns.
type DiagnoserFactory func(ns string) (Diagnoser, error)

// clusterDiagnoser is the real Diagnoser: the same wiring as agent-cli
// diagnose, with the read-only kubeconfig and a private evidence database.
type clusterDiagnoser struct {
	reg   *tools.Registry
	store *sqlite.Store
}

// newClusterDiagnoserFactory returns a factory that opens one evidence
// database per scenario under dbDir.
func newClusterDiagnoserFactory(agentKubeconfig, kubeContext, dbDir string) DiagnoserFactory {
	return func(ns string) (Diagnoser, error) {
		cs, err := kube.NewClientset(agentKubeconfig, kubeContext)
		if err != nil {
			return nil, err
		}
		// The allow list is the scenario namespace only, on top of the read-only
		// RBAC the agent kubeconfig already has.
		reader := kube.NewReader(cs, config.NamespaceFilters{Allow: []string{ns}})
		reg := tools.NewRegistry(false, k8stools.NewTools(reader)...)
		store, err := sqlite.Open(filepath.Join(dbDir, ns+".db"))
		if err != nil {
			return nil, fmt.Errorf("open evidence db: %w", err)
		}
		return &clusterDiagnoser{reg: reg, store: store}, nil
	}
}

func (d *clusterDiagnoser) Diagnose(ctx context.Context, opts investigation.Options) (*domain.Investigation, []domain.Evidence, error) {
	inv, err := investigation.Diagnose(ctx, d.reg, d.store, opts)
	if err != nil {
		return nil, nil, err
	}
	ev, err := d.store.List(ctx, inv.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list evidence: %w", err)
	}
	return inv, ev, nil
}

func (d *clusterDiagnoser) Close() error { return d.store.Close() }

// Runner evaluates scenarios one at a time.
type Runner struct {
	Driver      *Driver
	NewDiagnose DiagnoserFactory
	Clock       Clock
	// Interval is the pause between polls; zero means pollInterval.
	Interval    time.Duration
	KubeContext string
	Log         io.Writer
}

func (r *Runner) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return pollInterval
}

// RunAll evaluates the scenarios in order. It stops early only when ctx ends,
// in which case it returns what finished so far together with ctx's error.
func (r *Runner) RunAll(ctx context.Context, scs []Scenario) ([]Result, error) {
	var out []Result
	for _, sc := range scs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		fmt.Fprintf(r.Log, "==> %s\n", sc.ID)
		res := r.RunScenario(ctx, sc)
		out = append(out, res)
		fmt.Fprintf(r.Log, "    %s\n", res.Status)
	}
	return out, ctx.Err()
}

// RunScenario applies one scenario, waits for the symptom, scores it, and
// removes the namespace. It never returns an error: every failure becomes a
// failed Result so one broken scenario cannot stop the suite.
func (r *Runner) RunScenario(ctx context.Context, sc Scenario) (res Result) {
	if sc.LabOnly {
		return skippedResult(sc, "lab only")
	}
	start := r.Clock.Now()
	fail := func(format string, a ...any) Result {
		out := Score(sc, nil, nil)
		out.Detail = fmt.Sprintf(format, a...)
		return out
	}

	// Cleanup is registered before the first apply so a half-applied scenario
	// is removed too. It runs on a context that survives cancellation: Ctrl-C
	// is exactly when the lab most needs cleaning.
	defer func() {
		res.DurationMS = r.Clock.Now().Sub(start).Milliseconds()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := r.Driver.DeleteNamespace(cctx, sc.Namespace); err != nil {
			fmt.Fprintf(r.Log, "    cleanup of %s failed: %v\n", sc.Namespace, err)
			if res.Detail != "" {
				res.Detail += "; "
			}
			res.Detail += fmt.Sprintf("cleanup of %s failed: %v", sc.Namespace, err)
		}
	}()

	if err := r.Driver.WaitNamespaceGone(ctx, sc.Namespace, namespaceGoneTimeout); err != nil {
		return fail("namespace %s from a previous run is still being deleted: %v", sc.Namespace, err)
	}

	for i, file := range sc.Apply {
		if err := r.Driver.Apply(ctx, sc.Namespace, filepath.Join(sc.Dir, file)); err != nil {
			return fail("apply %s: %v", file, err)
		}
		// The fault step must start from a working state, so wait for the
		// previous step to roll out. The last step is the one under test and
		// is expected to stay broken.
		if i < len(sc.Apply)-1 {
			if err := r.Driver.WaitDeployments(ctx, sc.Namespace, rolloutTimeout); err != nil {
				return fail("wait for rollout after %s: %v", file, err)
			}
		}
	}

	d, err := r.NewDiagnose(sc.Namespace)
	if err != nil {
		return fail("set up diagnosis: %v", err)
	}
	defer d.Close()

	out := r.poll(ctx, sc, d)
	if out.ctxErr != nil {
		return fail("interrupted: %v", out.ctxErr)
	}
	res = Score(sc, out.inv, out.evidence)
	if out.inv == nil && out.diagErr != nil {
		res.Detail = fmt.Sprintf("diagnose: %v", out.diagErr)
	}
	if !out.satisfied && !sc.ExpectNoFindings {
		res.Detail = fmt.Sprintf("timed out after %ds; %s", sc.TimeoutSeconds, res.Detail)
	}
	return res
}

type pollOutcome struct {
	inv       *domain.Investigation
	evidence  []domain.Evidence
	satisfied bool  // every expected detector appeared
	diagErr   error // last Diagnose error, kept only for the detail text
	ctxErr    error
}

// poll runs Diagnose until every expected detector shows or the scenario
// timeout passes. A Diagnose error is not fatal: right after apply the
// workload may not exist yet. Negative scenarios have nothing to wait for, so
// they poll to the end and are judged on the last result.
func (r *Runner) poll(ctx context.Context, sc Scenario, d Diagnoser) pollOutcome {
	deadline := r.Clock.Now().Add(time.Duration(sc.TimeoutSeconds) * time.Second)
	opts := investigation.Options{Cluster: r.KubeContext, Kind: sc.Kind, Namespace: sc.Namespace, Name: sc.Name}
	var out pollOutcome
	for {
		inv, ev, err := d.Diagnose(ctx, opts)
		switch {
		case ctx.Err() != nil:
			out.ctxErr = ctx.Err()
			return out
		case err != nil:
			out.diagErr = err
		default:
			out.inv, out.evidence, out.diagErr = inv, ev, nil
			if !sc.ExpectNoFindings && hasDetectors(inv.Findings, sc.ExpectedDetectors) {
				out.satisfied = true
				return out
			}
		}
		// Stop when another full interval would pass the deadline.
		if r.Clock.Now().Add(r.interval()).After(deadline) {
			return out
		}
		if err := r.Clock.Sleep(ctx, r.interval()); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				out.ctxErr = err
			}
			return out
		}
	}
}

func hasDetectors(findings []domain.Finding, want []string) bool {
	have := map[string]bool{}
	for _, f := range findings {
		have[f.Detector] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}
