// Package kubernetes holds the target-cluster manifests for the triage agent
// and the tests that guard their security properties.
package kubernetes

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules   []rule    `yaml:"rules"`
	RoleRef roleRef   `yaml:"roleRef"`
	Subject []subject `yaml:"subjects"`
}

type rule struct {
	APIGroups       []string `yaml:"apiGroups"`
	Resources       []string `yaml:"resources"`
	NonResourceURLs []string `yaml:"nonResourceURLs"`
	Verbs           []string `yaml:"verbs"`
}

// hasNonResourceURLs reports whether any rule targets API server URLs such as
// /metrics instead of resources.
func hasNonResourceURLs(r object) bool {
	for _, ru := range r.Rules {
		if len(ru.NonResourceURLs) > 0 {
			return true
		}
	}
	return false
}

type roleRef struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type subject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

func loadObjects(t *testing.T) []object {
	t.Helper()
	data, err := os.ReadFile("rbac.yaml")
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var objs []object
	for {
		var o object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rbac.yaml: %v", err)
		}
		if o.Kind != "" {
			objs = append(objs, o)
		}
	}
	return objs
}

func find(objs []object, kind string) []object {
	var out []object
	for _, o := range objs {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

func clusterRole(t *testing.T) object {
	t.Helper()
	roles := find(loadObjects(t), "ClusterRole")
	if len(roles) != 1 {
		t.Fatalf("want exactly 1 ClusterRole, got %d", len(roles))
	}
	return roles[0]
}

// grants reports whether any rule grants verb on group/resource.
func grants(r object, group, resource, verb string) bool {
	for _, ru := range r.Rules {
		if slices.Contains(ru.APIGroups, group) &&
			slices.Contains(ru.Resources, resource) &&
			slices.Contains(ru.Verbs, verb) {
			return true
		}
	}
	return false
}

func TestNoSecretsAndNoWildcards(t *testing.T) {
	role := clusterRole(t)
	for _, ru := range role.Rules {
		for _, res := range ru.Resources {
			if res == "secrets" || res == "*" {
				t.Errorf("rule %v grants forbidden resource %q", ru.Resources, res)
			}
		}
		for _, g := range ru.APIGroups {
			if g == "*" {
				t.Errorf("rule %v uses wildcard apiGroup", ru.Resources)
			}
		}
		for _, v := range ru.Verbs {
			if v == "*" {
				t.Errorf("rule %v uses wildcard verb", ru.Resources)
			}
		}
	}
	for _, o := range loadObjects(t) {
		if o.Kind == "Secret" {
			t.Errorf("rbac.yaml must not contain Secret %q", o.Metadata.Name)
		}
	}
}

func TestOnlyReadVerbs(t *testing.T) {
	role := clusterRole(t)
	allowed := []string{"get", "list", "watch"}
	for _, ru := range role.Rules {
		if len(ru.Verbs) == 0 {
			t.Errorf("rule %v has no verbs", ru.Resources)
		}
		for _, v := range ru.Verbs {
			if !slices.Contains(allowed, v) {
				t.Errorf("rule %v grants non-read verb %q", ru.Resources, v)
			}
		}
	}
}

func TestNoExecPortforwardAttach(t *testing.T) {
	role := clusterRole(t)
	banned := []string{"pods/exec", "pods/portforward", "pods/attach", "nodes/proxy", "pods/proxy", "services/proxy"}
	for _, ru := range role.Rules {
		for _, res := range ru.Resources {
			if slices.Contains(banned, res) {
				t.Errorf("rule grants forbidden subresource %q", res)
			}
		}
	}
}

func TestPodLogsReadable(t *testing.T) {
	if !grants(clusterRole(t), "", "pods/log", "get") {
		t.Error("pods/log get is not granted")
	}
}

func TestReadSetCoverage(t *testing.T) {
	role := clusterRole(t)
	want := []struct{ group, resource string }{
		{"", "pods"},
		{"", "events"},
		{"", "nodes"},
		{"", "namespaces"},
		{"", "services"},
		{"", "persistentvolumeclaims"},
		{"discovery.k8s.io", "endpointslices"},
		{"apps", "deployments"},
		{"apps", "replicasets"},
		{"apps", "statefulsets"},
		{"apps", "daemonsets"},
		{"batch", "jobs"},
		{"batch", "cronjobs"},
		{"networking.k8s.io", "ingresses"},
		{"networking.k8s.io", "networkpolicies"},
		{"autoscaling", "horizontalpodautoscalers"},
		{"policy", "poddisruptionbudgets"},
	}
	for _, w := range want {
		for _, verb := range []string{"get", "list", "watch"} {
			if !grants(role, w.group, w.resource, verb) {
				t.Errorf("missing %s on %q/%s", verb, w.group, w.resource)
			}
		}
	}
}

// The role is applied in each target cluster and bound to whatever identity
// the off-cluster agent uses: a ServiceAccount token in dev, a Google identity
// (Workload Identity) in GKE.
func TestBindingCoversDevAndGoogleIdentity(t *testing.T) {
	objs := loadObjects(t)
	role := clusterRole(t)

	bindings := find(objs, "ClusterRoleBinding")
	if len(bindings) == 0 {
		t.Fatal("no ClusterRoleBinding")
	}
	var kinds []string
	for _, b := range bindings {
		if b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != role.Metadata.Name {
			t.Errorf("binding %q must reference ClusterRole %q, got %s/%s",
				b.Metadata.Name, role.Metadata.Name, b.RoleRef.Kind, b.RoleRef.Name)
		}
		for _, s := range b.Subject {
			kinds = append(kinds, s.Kind)
			if s.Kind == "ServiceAccount" && s.Namespace != "platform-agents" {
				t.Errorf("ServiceAccount subject %q must be in namespace platform-agents, got %q", s.Name, s.Namespace)
			}
		}
	}
	if !slices.Contains(kinds, "ServiceAccount") {
		t.Error("no ServiceAccount subject for the dev token identity")
	}
	if !slices.Contains(kinds, "User") {
		t.Error("no User subject for the Google identity used in GKE")
	}

	var sas []object
	for _, o := range find(objs, "ServiceAccount") {
		if o.Metadata.Namespace == "platform-agents" {
			sas = append(sas, o)
		}
	}
	if len(sas) == 0 {
		t.Error("no ServiceAccount in namespace platform-agents")
	}
}

// allowedReads is the complete set of group/resource pairs the role may grant,
// each with the reason it is needed. The test below compares the role to this
// set exactly, in both directions, so a new grant (ConfigMaps, ServiceAccounts,
// PersistentVolumes, a CRD group, a broad subresource, anything) fails until it
// is added here with a reason, in the same commit. ConfigMaps are deliberately
// absent: they routinely hold connection strings and tokens.
var allowedReads = map[string]string{
	"/pods":                                "workload state, container status, restarts",
	"/pods/log":                            "live logs for the pod under investigation",
	"/nodes":                               "node conditions and capacity",
	"/services":                            "selectors and ports",
	"/events":                              "the first thing to read for any failure",
	"/namespaces":                          "scope and namespace allow/deny",
	"/persistentvolumeclaims":              "pending or unbound volumes",
	"apps/deployments":                     "rollout state and revisions",
	"apps/replicasets":                     "owner chain and revision history",
	"apps/statefulsets":                    "ordered rollout state",
	"apps/daemonsets":                      "per-node rollout state",
	"batch/jobs":                           "job failures",
	"batch/cronjobs":                       "schedule and last run",
	"autoscaling/horizontalpodautoscalers": "saturation at maxReplicas",
	"policy/poddisruptionbudgets":          "blocked evictions and drains",
	"networking.k8s.io/ingresses":          "routing to services",
	"networking.k8s.io/networkpolicies":    "blocked traffic",
	"discovery.k8s.io/endpointslices":      "empty endpoints behind a failing readiness probe",

	// Beyond the core model, kept because they explain common failures.
	"/pods/status":         "pod conditions (PodScheduled, Ready) without parsing the whole spec",
	"/nodes/status":        "MemoryPressure, DiskPressure, PIDPressure, NotReady",
	"/endpoints":           "legacy Endpoints object, still what some controllers and probes read",
	"/resourcequotas":      "FailedCreate from an exceeded namespace quota",
	"/limitranges":         "default requests and limits that explain unexpected OOM or throttling",
	"metrics.k8s.io/pods":  "live CPU and memory (the kubectl top equivalent), get and list only",
	"metrics.k8s.io/nodes": "live node usage, get and list only",
}

func TestReadSetIsExactlyTheAllowList(t *testing.T) {
	role := clusterRole(t)
	granted := map[string]bool{}
	for _, ru := range role.Rules {
		for _, g := range ru.APIGroups {
			for _, res := range ru.Resources {
				granted[g+"/"+res] = true
			}
		}
	}
	for pair := range granted {
		if _, ok := allowedReads[pair]; !ok {
			t.Errorf("role grants %q, which is not in allowedReads; add it with a reason or remove the grant", pair)
		}
	}
	for pair := range allowedReads {
		if !granted[pair] {
			t.Errorf("allowedReads lists %q but the role does not grant it", pair)
		}
	}
}

// A rule with nonResourceURLs has no resources, so the allow-list above cannot
// see it. Granting get on "*" or "/metrics" would pass every other check, so
// the role must not contain any.
func TestNoNonResourceURLs(t *testing.T) {
	if hasNonResourceURLs(clusterRole(t)) {
		t.Fatal("ClusterRole must not grant nonResourceURLs")
	}
}

func TestHasNonResourceURLsDetectsThem(t *testing.T) {
	var role object
	err := yaml.Unmarshal([]byte(`
kind: ClusterRole
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get]
  - nonResourceURLs: ["/metrics"]
    verbs: [get]
`), &role)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNonResourceURLs(role) {
		t.Fatal("a rule with nonResourceURLs was not detected")
	}

	var clean object
	if err := yaml.Unmarshal([]byte("kind: ClusterRole\nrules:\n  - apiGroups: ['']\n    resources: [pods]\n    verbs: [get]\n"), &clean); err != nil {
		t.Fatal(err)
	}
	if hasNonResourceURLs(clean) {
		t.Fatal("false positive on a role without nonResourceURLs")
	}
}
