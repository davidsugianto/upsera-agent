package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// kindContextPrefix is what a kind cluster's kubeconfig context is named. It is
// the only evidence the harness has that a context is a throwaway cluster, so
// anything else is refused.
const kindContextPrefix = "kind-"

// Kubectl runs one kubectl command and returns its combined output. It is the
// seam between the driver and the real binary, so tests can record the
// commands instead of running them.
type Kubectl interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// execKubectl runs the kubectl binary found on PATH.
type execKubectl struct{}

func (execKubectl) Run(ctx context.Context, args ...string) (string, error) {
	// Every argument list is built by Driver, which pins kubeconfig and context.
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput() //nolint:gosec // args come from Driver only
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("kubectl: %w: %s", err, text)
		}
		return text, fmt.Errorf("kubectl: %w", err)
	}
	return text, nil
}

// Driver applies scenarios to the lab cluster and removes them again. It is
// the only code in the harness that talks to the cluster as admin, so it holds
// the safety rules:
//   - the context must be a kind context, checked before any command exists;
//   - every command passes --kubeconfig and --context explicitly, so the
//     current context of the user's kubeconfig is never consulted;
//   - only namespaces starting with "scenario-" are touched.
type Driver struct {
	k           Kubectl
	kubeconfig  string
	kubeContext string
}

// NewDriver validates the target cluster. No command runs on failure.
func NewDriver(k Kubectl, kubeconfig, kubeContext string) (*Driver, error) {
	if !strings.HasPrefix(kubeContext, kindContextPrefix) {
		return nil, fmt.Errorf("refusing to use context %q: the eval harness only runs against a kind cluster (context must start with %q)",
			kubeContext, kindContextPrefix)
	}
	if kubeconfig == "" {
		return nil, errors.New("an admin kubeconfig is required (--admin-kubeconfig)")
	}
	return &Driver{k: k, kubeconfig: kubeconfig, kubeContext: kubeContext}, nil
}

// run prepends the flags that pin the cluster.
func (d *Driver) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", d.kubeconfig, "--context", d.kubeContext}, args...)
	return d.k.Run(ctx, full...)
}

func checkScenarioNamespace(ns string) error {
	if !strings.HasPrefix(ns, namespacePrefix) || len(ns) == len(namespacePrefix) {
		return fmt.Errorf("refusing to touch namespace %q: only %q namespaces are allowed", ns, namespacePrefix)
	}
	return nil
}

// Apply applies one manifest file into the scenario namespace.
func (d *Driver) Apply(ctx context.Context, ns, file string) error {
	if err := checkScenarioNamespace(ns); err != nil {
		return err
	}
	_, err := d.run(ctx, "apply", "-n", ns, "-f", file)
	return err
}

// WaitDeployments blocks until every Deployment in the namespace has rolled
// out. A scenario whose first step is meant to be healthy uses it to know the
// fault step starts from a working state.
func (d *Driver) WaitDeployments(ctx context.Context, ns string, timeout time.Duration) error {
	if err := checkScenarioNamespace(ns); err != nil {
		return err
	}
	out, err := d.run(ctx, "get", "deployments", "-n", ns, "-o", "name")
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(out) {
		if _, err := d.run(ctx, "rollout", "status", name, "-n", ns, fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
			return err
		}
	}
	return nil
}

// WaitNamespaceGone blocks until the namespace no longer exists. Cleanup
// deletes without waiting, so a rerun can start while the previous run's
// namespace is still terminating: kubectl apply then reports "unchanged" and
// the objects disappear underneath the scenario. A namespace that does not
// exist returns at once.
func (d *Driver) WaitNamespaceGone(ctx context.Context, ns string, timeout time.Duration) error {
	if err := checkScenarioNamespace(ns); err != nil {
		return err
	}
	_, err := d.run(ctx, "wait", "--for=delete", "namespace/"+ns, fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	return err
}

// DeleteNamespace removes the scenario namespace without waiting for it to
// terminate. A namespace that is already gone is not an error.
func (d *Driver) DeleteNamespace(ctx context.Context, ns string) error {
	if err := checkScenarioNamespace(ns); err != nil {
		return err
	}
	_, err := d.run(ctx, "delete", "namespace", ns, "--wait=false", "--ignore-not-found")
	return err
}
