package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/davidsugianto/upsera-agent/internal/config"
)

func TestAllowed(t *testing.T) {
	r := NewReader(fake.NewClientset(), config.NamespaceFilters{Allow: []string{"shop", "kube-system"}, Deny: []string{"kube-system"}})
	for ns, wantErr := range map[string]bool{"shop": false, "kube-system": true, "other": true, "": true} {
		err := r.Allowed(ns)
		if (err != nil) != wantErr {
			t.Errorf("Allowed(%q) err=%v, wantErr=%v", ns, err, wantErr)
		}
		if err != nil && !errors.Is(err, ErrNamespaceDenied) {
			t.Errorf("Allowed(%q) error does not wrap ErrNamespaceDenied: %v", ns, err)
		}
	}
	open := NewReader(fake.NewClientset(), config.NamespaceFilters{Deny: []string{"kube-system"}})
	if err := open.Allowed("anything"); err != nil {
		t.Errorf("empty allow list should permit: %v", err)
	}
}

func TestDeniedNamespaceNeverReachesCluster(t *testing.T) {
	cs := fake.NewClientset()
	r := NewReader(cs, config.NamespaceFilters{Deny: []string{"kube-system"}})
	ctx := context.Background()
	if _, err := r.ListPods(ctx, "kube-system", map[string]string{"a": "b"}); !errors.Is(err, ErrNamespaceDenied) {
		t.Fatalf("ListPods: %v", err)
	}
	if _, err := r.PodLogs(ctx, "kube-system", "p", "c", false, 10); !errors.Is(err, ErrNamespaceDenied) {
		t.Fatalf("PodLogs: %v", err)
	}
	if _, err := r.ListEvents(ctx, "kube-system", "p"); !errors.Is(err, ErrNamespaceDenied) {
		t.Fatalf("ListEvents: %v", err)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Fatalf("denied reads reached the API %d times", n)
	}
}

func TestListPodsRefusesEmptySelector(t *testing.T) {
	r := NewReader(fake.NewClientset(), config.NamespaceFilters{})
	if _, err := r.ListPods(context.Background(), "shop", nil); err == nil {
		t.Fatal("expected error for empty selector")
	}
}

func TestGetWorkloadAndHistory(t *testing.T) {
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", UID: "u1", Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, ObservedGeneration: 3, Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}},
	}
	mkRS := func(name, rev, image string, owned bool) *appsv1.ReplicaSet {
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "api"},
			Annotations: map[string]string{"deployment.kubernetes.io/revision": rev}}}
		rs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: image}}
		if owned {
			rs.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "u1"}}
		}
		return rs
	}
	r := NewReader(fake.NewClientset(dep, mkRS("api-1", "1", "img:1", true), mkRS("api-2", "2", "img:2", true), mkRS("stray", "9", "img:9", false)), config.NamespaceFilters{})
	w, err := r.GetWorkload(context.Background(), "deploy", "shop", "api")
	if err != nil {
		t.Fatal(err)
	}
	if w.Desired != 2 || w.Ready != 1 || w.Selector["app"] != "api" || len(w.Conditions) != 1 {
		t.Fatalf("unexpected workload: %+v", w)
	}
	h, err := r.RolloutHistory(context.Background(), "shop", "api")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].Revision != 2 || !strings.Contains(h[0].Images[0], "img:2") {
		t.Fatalf("unexpected history: %+v", h)
	}
}

func TestNormalizeKind(t *testing.T) {
	for in, want := range map[string]string{"deploy": "deployment", "STS": "statefulset", "ds": "daemonset", "no": "node", "po": "pod"} {
		if got := NormalizeKind(in); got != want {
			t.Errorf("NormalizeKind(%q)=%q want %q", in, got, want)
		}
	}
}

func TestNewClientsetRequiresContext(t *testing.T) {
	if _, err := NewClientset("", ""); err == nil {
		t.Fatal("expected error without a context")
	}
}

func TestCheckNamespaceNeedsNoReader(t *testing.T) {
	f := config.NamespaceFilters{Deny: []string{"kube-system"}}
	if err := CheckNamespace(f, "shop"); err != nil {
		t.Fatalf("shop: %v", err)
	}
	if err := CheckNamespace(f, "kube-system"); !errors.Is(err, ErrNamespaceDenied) {
		t.Fatalf("kube-system: %v", err)
	}
	// Reader.Allowed must agree with the standalone check.
	r := NewReader(fake.NewClientset(), f)
	if (r.Allowed("kube-system") == nil) != (CheckNamespace(f, "kube-system") == nil) {
		t.Fatal("Reader.Allowed and CheckNamespace disagree")
	}
}
