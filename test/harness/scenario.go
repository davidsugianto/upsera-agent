package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	k8stools "github.com/davidsugianto/upsera-agent/internal/agent/tools/k8s"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

const (
	// namespacePrefix is the only kind of namespace the harness will create,
	// diagnose or delete. It is the guard that keeps a typo in a fixture from
	// pointing the cleanup at a real namespace.
	namespacePrefix = "scenario-"

	defaultTimeoutSeconds = 240
	defaultManifest       = "manifest.yaml"
)

// knownDetectors mirrors the detector names in internal/usecase/investigation/detect.
// They are unexported there, but the package documents them as the contract
// with this harness, so they are repeated here and checked by a test.
var knownDetectors = []string{
	"crashloop", "oomkilled", "image_pull", "unschedulable",
	"probe_failure", "node_not_ready", "evicted", "rollout_stuck",
}

// Scenario is the part of expected.yaml the harness acts on. The file carries
// more fields (root_cause, must_mention, ...) for the later LLM scoring; they
// are ignored here.
type Scenario struct {
	ID                 string   `yaml:"id"`
	Namespace          string   `yaml:"namespace"`
	Target             string   `yaml:"target"`
	Apply              []string `yaml:"apply"`
	TimeoutSeconds     int      `yaml:"timeout_seconds"`
	ExpectedDetectors  []string `yaml:"expected_detectors"`
	ForbiddenDetectors []string `yaml:"forbidden_detectors"`
	CauseTerms         []string `yaml:"cause_terms"`
	ExpectNoFindings   bool     `yaml:"expect_no_findings"`
	LabOnly            bool     `yaml:"lab_only"`
	// RequiredEvidenceFrom lists tools; in LLM mode the rank-1 hypothesis must
	// cite evidence from at least one of them. Empty means no requirement.
	RequiredEvidenceFrom []string `yaml:"required_evidence_from"`

	// Dir is the scenario directory; Kind and Name are Target, normalised.
	Dir  string `yaml:"-"`
	Kind string `yaml:"-"`
	Name string `yaml:"-"`
}

// LoadScenarios parses every scenario directory under root. It returns all the
// scenarios that parsed and one error per problem found, so --validate can
// report everything in one pass. Scenarios are ordered by directory name.
func LoadScenarios(root string) ([]Scenario, []error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, []error{fmt.Errorf("read scenarios dir: %w", err)}
	}
	var (
		out  []Scenario
		errs []error
	)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sc, serrs := loadScenario(filepath.Join(root, e.Name()))
		for _, se := range serrs {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), se))
		}
		if len(serrs) == 0 {
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}

func loadScenario(dir string) (Scenario, []error) {
	var sc Scenario
	b, err := os.ReadFile(filepath.Join(dir, "expected.yaml")) //nolint:gosec // dir comes from the --scenarios flag
	if err != nil {
		return sc, []error{fmt.Errorf("read expected.yaml: %w", err)}
	}
	if err := yaml.Unmarshal(b, &sc); err != nil {
		return sc, []error{fmt.Errorf("parse expected.yaml: %w", err)}
	}
	sc.Dir = dir
	if len(sc.Apply) == 0 {
		sc.Apply = []string{defaultManifest}
	}
	if sc.TimeoutSeconds == 0 {
		sc.TimeoutSeconds = defaultTimeoutSeconds
	}
	return sc, sc.validate()
}

func (sc *Scenario) validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case sc.ID == "":
		bad("id is required")
	case sc.ID != filepath.Base(sc.Dir):
		bad("id %q must equal the directory name %q", sc.ID, filepath.Base(sc.Dir))
	}

	switch {
	case sc.Namespace == "":
		bad("namespace is required")
	case !strings.HasPrefix(sc.Namespace, namespacePrefix) || len(sc.Namespace) == len(namespacePrefix):
		bad("namespace %q must start with %q", sc.Namespace, namespacePrefix)
	}

	if sc.Target == "" {
		bad("target is required")
	} else if k, n, err := parseTarget(sc.Target); err != nil {
		bad("%v", err)
	} else {
		sc.Kind, sc.Name = k, n
	}

	if sc.TimeoutSeconds < 0 {
		bad("timeout_seconds must not be negative")
	}

	for _, f := range sc.Apply {
		// A bare file name keeps every apply inside the scenario directory.
		if f == "" || f != filepath.Base(f) {
			bad("apply entry %q must be a file name in the scenario directory", f)
			continue
		}
		path := filepath.Join(sc.Dir, f)
		if _, err := os.Stat(path); err != nil {
			bad("apply file %s not found", f)
			continue
		}
		if err := checkManifestNamespaces(path, sc.Namespace); err != nil {
			bad("%s: %v", f, err)
		}
	}

	for _, d := range sc.ExpectedDetectors {
		if !isKnownDetector(d) {
			bad("expected_detectors: unknown detector %q", d)
		}
	}
	for _, d := range sc.ForbiddenDetectors {
		if !isKnownDetector(d) {
			bad("forbidden_detectors: unknown detector %q", d)
		}
	}
	for _, name := range sc.RequiredEvidenceFrom {
		if !isKnownTool(name) {
			bad("required_evidence_from: unknown tool %q (want one of %s)", name, strings.Join(knownTools, ", "))
		}
	}
	switch {
	case sc.ExpectNoFindings && len(sc.ExpectedDetectors) > 0:
		bad("expect_no_findings cannot be combined with expected_detectors")
	case !sc.ExpectNoFindings && !sc.LabOnly && len(sc.ExpectedDetectors) == 0:
		// Without this a scenario would pass vacuously.
		bad("expected_detectors is required unless expect_no_findings is true")
	}
	return errs
}

// knownTools are the read-only tool names evidence rows are tagged with.
var knownTools = []string{
	k8stools.NameGet, k8stools.NameListPods, k8stools.NameDescribe,
	k8stools.NameEvents, k8stools.NamePodLogs, k8stools.NameRolloutHistory,
}

func isKnownTool(name string) bool {
	for _, t := range knownTools {
		if t == name {
			return true
		}
	}
	return false
}

func isKnownDetector(name string) bool {
	for _, d := range knownDetectors {
		if d == name {
			return true
		}
	}
	return false
}

// parseTarget splits <kind>/<name> and normalises the kind, the same way
// agent-cli diagnose does.
func parseTarget(s string) (kind, name string, err error) {
	k, n, ok := strings.Cut(s, "/")
	if !ok || k == "" || n == "" || strings.Contains(n, "/") {
		return "", "", fmt.Errorf("target %q must look like <kind>/<name>", s)
	}
	kind = kube.NormalizeKind(k)
	for _, supported := range kube.WorkloadKinds {
		if kind == supported {
			return kind, n, nil
		}
	}
	return "", "", fmt.Errorf("target kind %q must be one of %s", k, strings.Join(kube.WorkloadKinds, ", "))
}

// allowedKinds are the namespaced kinds a scenario may create. The driver
// applies fixtures as cluster admin and cleanup deletes only the namespace, so
// anything cluster scoped (ClusterRoleBinding, CRDs, PersistentVolumes,
// webhooks) would outlive the run. An allow-list also fails closed on kinds
// nobody has reviewed. Add a kind here only if it is namespaced.
var allowedKinds = map[string]bool{
	"Namespace": true, "Deployment": true, "StatefulSet": true, "DaemonSet": true,
	"ReplicaSet": true, "Job": true, "CronJob": true, "Pod": true, "Service": true,
	"ConfigMap": true, "Secret": true, "ServiceAccount": true, "Role": true,
	"RoleBinding": true, "NetworkPolicy": true, "Ingress": true,
	"HorizontalPodAutoscaler": true, "PodDisruptionBudget": true,
	"PersistentVolumeClaim": true, "ResourceQuota": true, "LimitRange": true,
}

// checkManifestNamespaces refuses a manifest that would create a Namespace
// other than the scenario's own, or an object of a kind outside allowedKinds.
// Namespaced objects that name a different namespace are rejected by
// kubectl apply -n itself.
func checkManifestNamespaces(path, want string) error {
	b, err := os.ReadFile(path) //nolint:gosec // path is inside the scenario directory
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("parse: %w", err)
		}
		if doc.Kind == "" {
			continue
		}
		if !allowedKinds[doc.Kind] {
			return fmt.Errorf("kind %s is not allowed in a scenario (only namespaced kinds in allowedKinds)", doc.Kind)
		}
		if doc.Kind == "Namespace" && doc.Metadata.Name != want {
			return fmt.Errorf("creates Namespace %q; only %q is allowed", doc.Metadata.Name, want)
		}
	}
}
