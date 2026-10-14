package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKubectl records every command instead of running it. fn, when set,
// decides the result.
type fakeKubectl struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(ctx context.Context, args []string) (string, error)
}

func (f *fakeKubectl) Run(ctx context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, args)
	}
	return "", nil
}

func (f *fakeKubectl) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func newTestDriver(t *testing.T, f *fakeKubectl) *Driver {
	t.Helper()
	d, err := NewDriver(f, "/tmp/admin.kubeconfig", "kind-upsera-lab")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewDriverRefusesNonKindContext(t *testing.T) {
	for _, ctx := range []string{"", "gke_prod_europe_cluster", "prod", "kind", "minikube", "docker-desktop", "Kind-x"} {
		t.Run(ctx, func(t *testing.T) {
			f := &fakeKubectl{}
			d, err := NewDriver(f, "/tmp/admin.kubeconfig", ctx)
			if err == nil || d != nil {
				t.Fatalf("context %q must be refused", ctx)
			}
			if len(f.calls) != 0 {
				t.Errorf("commands ran before the refusal: %v", f.recorded())
			}
		})
	}
}

func TestNewDriverRequiresKubeconfig(t *testing.T) {
	if _, err := NewDriver(&fakeKubectl{}, "", "kind-upsera-lab"); err == nil {
		t.Error("an empty admin kubeconfig must be refused so kubectl never falls back to ~/.kube/config")
	}
}

func TestDriverPinsKubeconfigAndContextOnEveryCommand(t *testing.T) {
	f := &fakeKubectl{fn: func(_ context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "get deployments") {
			return "deployment.apps/web\ndeployment.apps/worker\n", nil
		}
		return "", nil
	}}
	d := newTestDriver(t, f)
	ctx := context.Background()
	if err := d.Apply(ctx, "scenario-x", "/s/manifest.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := d.WaitDeployments(ctx, "scenario-x", 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteNamespace(ctx, "scenario-x"); err != nil {
		t.Fatal(err)
	}

	if len(f.calls) != 5 {
		t.Fatalf("calls = %v", f.recorded())
	}
	for _, c := range f.calls {
		if len(c) < 5 || c[0] != "--kubeconfig" || c[1] != "/tmp/admin.kubeconfig" || c[2] != "--context" || c[3] != "kind-upsera-lab" {
			t.Errorf("command does not pin kubeconfig and context first: %v", c)
		}
		if c[4] == "config" || strings.Contains(strings.Join(c, " "), "use-context") {
			t.Errorf("harness must never touch kubeconfig state: %v", c)
		}
	}
	want := []string{
		"--kubeconfig /tmp/admin.kubeconfig --context kind-upsera-lab apply -n scenario-x -f /s/manifest.yaml",
		"--kubeconfig /tmp/admin.kubeconfig --context kind-upsera-lab get deployments -n scenario-x -o name",
		"--kubeconfig /tmp/admin.kubeconfig --context kind-upsera-lab rollout status deployment.apps/web -n scenario-x --timeout=90s",
		"--kubeconfig /tmp/admin.kubeconfig --context kind-upsera-lab rollout status deployment.apps/worker -n scenario-x --timeout=90s",
		"--kubeconfig /tmp/admin.kubeconfig --context kind-upsera-lab delete namespace scenario-x --wait=false --ignore-not-found",
	}
	for i, w := range want {
		if got := f.recorded()[i]; got != w {
			t.Errorf("call %d = %q, want %q", i, got, w)
		}
	}
}

func TestDriverRefusesNonScenarioNamespaces(t *testing.T) {
	for _, ns := range []string{"", "default", "kube-system", "production", "scenario-", "myscenario-x", "Scenario-x"} {
		t.Run(ns, func(t *testing.T) {
			f := &fakeKubectl{}
			d := newTestDriver(t, f)
			ctx := context.Background()
			if err := d.Apply(ctx, ns, "m.yaml"); err == nil {
				t.Error("Apply must refuse")
			}
			if err := d.WaitDeployments(ctx, ns, time.Second); err == nil {
				t.Error("WaitDeployments must refuse")
			}
			if err := d.DeleteNamespace(ctx, ns); err == nil {
				t.Error("DeleteNamespace must refuse")
			}
			if err := d.WaitNamespaceGone(ctx, ns, time.Second); err == nil {
				t.Error("WaitNamespaceGone must refuse")
			}
			if len(f.calls) != 0 {
				t.Errorf("a command ran for namespace %q: %v", ns, f.recorded())
			}
		})
	}
}

func TestDriverPropagatesKubectlErrors(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeKubectl{fn: func(context.Context, []string) (string, error) { return "", boom }}
	d := newTestDriver(t, f)
	ctx := context.Background()
	if err := d.Apply(ctx, "scenario-x", "m.yaml"); !errors.Is(err, boom) {
		t.Errorf("Apply error = %v", err)
	}
	if err := d.WaitDeployments(ctx, "scenario-x", time.Second); !errors.Is(err, boom) {
		t.Errorf("WaitDeployments error = %v", err)
	}
	if err := d.DeleteNamespace(ctx, "scenario-x"); !errors.Is(err, boom) {
		t.Errorf("DeleteNamespace error = %v", err)
	}
}

func TestWaitDeploymentsStopsAtFirstFailedRollout(t *testing.T) {
	f := &fakeKubectl{fn: func(_ context.Context, args []string) (string, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "get deployments"):
			return "deployment.apps/a deployment.apps/b", nil
		case strings.Contains(j, "rollout status deployment.apps/a"):
			return "", errors.New("timed out")
		}
		return "", nil
	}}
	d := newTestDriver(t, f)
	if err := d.WaitDeployments(context.Background(), "scenario-x", time.Second); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(f.calls); n != 2 {
		t.Errorf("calls = %v, want get + one rollout status", f.recorded())
	}
}
