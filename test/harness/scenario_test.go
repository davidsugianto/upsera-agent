package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validExpected = `id: oomkilled
namespace: scenario-oomkilled
target: deployment/memory-hog
expected_detectors: [oomkilled]
cause_terms: ["memory limit"]
`

// writeScenario creates root/<id>/ with expected.yaml, manifest.yaml and any
// extra files, and returns root.
func writeScenario(t *testing.T, root, dir, expected string, extra ...string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("expected.yaml", expected)
	write("manifest.yaml", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespaceOf(expected)+"\n")
	for _, name := range extra {
		write(name, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	}
	return root
}

func namespaceOf(expected string) string {
	for _, l := range strings.Split(expected, "\n") {
		if v, ok := strings.CutPrefix(l, "namespace: "); ok {
			return v
		}
	}
	return "scenario-x"
}

func errText(errs []error) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "\n")
}

func TestLoadScenarioValid(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	scs, errs := LoadScenarios(root)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(scs) != 1 {
		t.Fatalf("got %d scenarios", len(scs))
	}
	sc := scs[0]
	if sc.Kind != "deployment" || sc.Name != "memory-hog" {
		t.Errorf("target = %s/%s", sc.Kind, sc.Name)
	}
	if sc.TimeoutSeconds != 240 || len(sc.Apply) != 1 || sc.Apply[0] != "manifest.yaml" {
		t.Errorf("defaults not applied: timeout=%d apply=%v", sc.TimeoutSeconds, sc.Apply)
	}
}

func TestLoadScenarioShortTargetKind(t *testing.T) {
	exp := strings.Replace(validExpected, "deployment/memory-hog", "sts/db", 1)
	scs, errs := LoadScenarios(writeScenario(t, t.TempDir(), "oomkilled", exp))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if scs[0].Kind != "statefulset" {
		t.Errorf("kind = %q, want statefulset", scs[0].Kind)
	}
}

func TestLoadScenarioErrors(t *testing.T) {
	tests := []struct {
		name     string
		dir      string
		expected string
		extra    []string
		want     string
	}{
		{"missing id", "oomkilled", strings.Replace(validExpected, "id: oomkilled\n", "", 1), nil, "id is required"},
		{"id differs from directory", "other", validExpected, nil, `must equal the directory name "other"`},
		{"missing namespace", "oomkilled", strings.Replace(validExpected, "namespace: scenario-oomkilled\n", "", 1), nil, "namespace is required"},
		{"bad namespace prefix", "oomkilled", strings.Replace(validExpected, "scenario-oomkilled", "kube-system", 1), nil, `must start with "scenario-"`},
		{"bare prefix namespace", "oomkilled", strings.Replace(validExpected, "scenario-oomkilled", "scenario-", 1), nil, `must start with "scenario-"`},
		{"missing target", "oomkilled", strings.Replace(validExpected, "target: deployment/memory-hog\n", "", 1), nil, "target is required"},
		{"target without name", "oomkilled", strings.Replace(validExpected, "deployment/memory-hog", "deployment", 1), nil, "must look like <kind>/<name>"},
		{"target kind", "oomkilled", strings.Replace(validExpected, "deployment/memory-hog", "pod/x", 1), nil, "must be one of"},
		{"unknown detector", "oomkilled", strings.Replace(validExpected, "[oomkilled]", "[oomkiled]", 1), nil, `unknown detector "oomkiled"`},
		{"unknown forbidden detector", "oomkilled", validExpected + "forbidden_detectors: [nope]\n", nil, `unknown detector "nope"`},
		{"missing apply file", "oomkilled", validExpected + "apply: [manifest.yaml, fault.yaml]\n", nil, "apply file fault.yaml not found"},
		{"apply path traversal", "oomkilled", validExpected + "apply: [../manifest.yaml]\n", nil, "must be a file name"},
		{"no expected detectors", "oomkilled", strings.Replace(validExpected, "expected_detectors: [oomkilled]\n", "", 1), nil, "expected_detectors is required"},
		{"no-findings with detectors", "oomkilled", validExpected + "expect_no_findings: true\n", nil, "cannot be combined"},
		{"negative timeout", "oomkilled", validExpected + "timeout_seconds: -1\n", nil, "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := LoadScenarios(writeScenario(t, t.TempDir(), tt.dir, tt.expected, tt.extra...))
			if len(errs) == 0 {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if got := errText(errs); !strings.Contains(got, tt.want) {
				t.Errorf("errors = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestLoadScenarioRejectsForeignNamespaceObject(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	manifest := filepath.Join(root, "oomkilled", "manifest.yaml")
	body := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: scenario-oomkilled\n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: production\n"
	if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errs := LoadScenarios(root)
	if got := errText(errs); !strings.Contains(got, `creates Namespace "production"`) {
		t.Errorf("errors = %q", got)
	}
}

func TestLoadScenarioMissingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	_, errs := LoadScenarios(root)
	if got := errText(errs); !strings.Contains(got, "empty: read expected.yaml") {
		t.Errorf("errors = %q", got)
	}

	root = writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	if err := os.Remove(filepath.Join(root, "oomkilled", "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	_, errs = LoadScenarios(root)
	if got := errText(errs); !strings.Contains(got, "apply file manifest.yaml not found") {
		t.Errorf("errors = %q", got)
	}
}

func TestLoadScenarioReportsEveryProblem(t *testing.T) {
	exp := "id: wrong\nnamespace: prod\ntarget: nope\n"
	_, errs := LoadScenarios(writeScenario(t, t.TempDir(), "dir", exp))
	if len(errs) < 3 {
		t.Errorf("got %d errors, want all problems reported: %v", len(errs), errs)
	}
}

func TestLoadScenariosSkipsHiddenAndFiles(t *testing.T) {
	root := writeScenario(t, t.TempDir(), "oomkilled", validExpected)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	scs, errs := LoadScenarios(root)
	if len(errs) != 0 || len(scs) != 1 {
		t.Errorf("scenarios=%d errs=%v", len(scs), errs)
	}
}

func TestLoadScenariosMissingRoot(t *testing.T) {
	_, errs := LoadScenarios(filepath.Join(t.TempDir(), "nope"))
	if len(errs) != 1 {
		t.Errorf("errs = %v", errs)
	}
}

// The detector names are unexported in package detect, so the harness repeats
// them. This reads the source of record to catch a rename or a new detector.
func TestKnownDetectorsMatchDetectPackage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "usecase", "investigation", "detect", "run.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, d := range knownDetectors {
		if !strings.Contains(src, `"`+d+`"`) {
			t.Errorf("detector %q is not defined in detect/run.go", d)
		}
	}
	if got := strings.Count(src, "= \""); got != len(knownDetectors) {
		t.Errorf("detect/run.go defines %d detector names, harness knows %d", got, len(knownDetectors))
	}
}

// TestRealScenariosValidate keeps the checked-in fixtures loadable, so a broken
// fixture fails in unit tests and not only in `make eval`.
func TestRealScenariosValidate(t *testing.T) {
	scs, errs := LoadScenarios(filepath.Join("..", "scenarios"))
	if len(errs) > 0 {
		t.Fatalf("scenario errors:\n%s", errText(errs))
	}
	if len(scs) == 0 {
		t.Fatal("no scenarios found in ../scenarios")
	}
}

func TestCheckManifestKindsAllowList(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "manifest.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ok := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: scenario-x\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n"
	if err := checkManifestNamespaces(write(t, ok), "scenario-x"); err != nil {
		t.Fatalf("namespaced kinds must pass: %v", err)
	}
	// Cleanup deletes only the namespace, so anything cluster scoped would be
	// applied as admin and left behind.
	for _, kind := range []string{"ClusterRoleBinding", "ClusterRole", "CustomResourceDefinition", "PersistentVolume", "StorageClass", "ValidatingWebhookConfiguration", "Node", "SomethingUnknown"} {
		body := ok + "---\napiVersion: x/v1\nkind: " + kind + "\nmetadata:\n  name: evil\n"
		err := checkManifestNamespaces(write(t, body), "scenario-x")
		if err == nil || !strings.Contains(err.Error(), kind) {
			t.Errorf("kind %s: err = %v, want a refusal naming the kind", kind, err)
		}
	}
}
