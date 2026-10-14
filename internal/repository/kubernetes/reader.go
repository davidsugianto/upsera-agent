// Package kubernetes is the read-only gateway to a target cluster. Every call
// goes through the namespace filter, so no tool can read a namespace the
// configuration denies. It uses only get, list, and log reads, which is what
// the reader ClusterRole grants (deploy/kubernetes/rbac.yaml).
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/davidsugianto/upsera-agent/internal/config"
)

// ErrNamespaceDenied is returned for a namespace the configuration forbids.
var ErrNamespaceDenied = errors.New("namespace not allowed by configuration")

// maxLogBytes bounds how much of a log tail is ever read into memory.
const maxLogBytes = 64 << 10

// Reader reads one target cluster.
type Reader struct {
	cs     k8s.Interface
	filter config.NamespaceFilters
}

// NewReader wraps a clientset with the namespace filter.
func NewReader(cs k8s.Interface, f config.NamespaceFilters) *Reader {
	return &Reader{cs: cs, filter: f}
}

// NewClientset builds a clientset for an explicit kubeconfig context. It never
// falls back to the kubeconfig's current context, so a stray `kubectl config
// use-context` cannot redirect the agent. An empty path uses KUBECONFIG or the
// default location.
func NewClientset(kubeconfigPath, kubeContext string) (k8s.Interface, error) {
	if kubeContext == "" {
		return nil, errors.New("kubeconfig context is required")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig context %q: %w", kubeContext, err)
	}
	return k8s.NewForConfig(cfg)
}

// Allowed reports whether the namespace may be read; see CheckNamespace.
func (r *Reader) Allowed(ns string) error { return CheckNamespace(r.filter, ns) }

// CheckNamespace applies the namespace filter without needing a cluster
// connection, so callers can refuse a namespace before building a client. Deny
// wins over allow, and an empty allow list permits everything not denied.
func CheckNamespace(f config.NamespaceFilters, ns string) error {
	if ns == "" {
		return fmt.Errorf("%w: namespace is empty", ErrNamespaceDenied)
	}
	for _, d := range f.Deny {
		if d == ns {
			return fmt.Errorf("%w: %q is denied", ErrNamespaceDenied, ns)
		}
	}
	if len(f.Allow) == 0 {
		return nil
	}
	for _, a := range f.Allow {
		if a == ns {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not in the allow list", ErrNamespaceDenied, ns)
}

// Condition is a status condition reduced to the fields detectors use.
type Condition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// Workload is a Deployment, StatefulSet, or DaemonSet reduced to what the
// diagnosis needs.
type Workload struct {
	Kind               string
	Namespace          string
	Name               string
	Selector           map[string]string
	Desired            int32
	Ready              int32
	Updated            int32
	Generation         int64
	ObservedGeneration int64
	Conditions         []Condition
}

// WorkloadKinds lists the kinds GetWorkload accepts, normalised.
var WorkloadKinds = []string{"deployment", "statefulset", "daemonset"}

// NormalizeKind maps kubectl-style short names to the canonical kind.
func NormalizeKind(k string) string {
	switch strings.ToLower(k) {
	case "deploy", "deployment", "deployments":
		return "deployment"
	case "sts", "statefulset", "statefulsets":
		return "statefulset"
	case "ds", "daemonset", "daemonsets":
		return "daemonset"
	case "node", "nodes", "no":
		return "node"
	case "pod", "pods", "po":
		return "pod"
	}
	return strings.ToLower(k)
}

// GetWorkload reads a Deployment, StatefulSet, or DaemonSet.
func (r *Reader) GetWorkload(ctx context.Context, kind, ns, name string) (*Workload, error) {
	if err := r.Allowed(ns); err != nil {
		return nil, err
	}
	switch NormalizeKind(kind) {
	case "deployment":
		d, err := r.cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		w := &Workload{Kind: "Deployment", Namespace: ns, Name: name, Desired: 1,
			Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas,
			Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration}
		if d.Spec.Replicas != nil {
			w.Desired = *d.Spec.Replicas
		}
		if d.Spec.Selector != nil {
			w.Selector = d.Spec.Selector.MatchLabels
		}
		for _, c := range d.Status.Conditions {
			w.Conditions = append(w.Conditions, Condition{string(c.Type), string(c.Status), c.Reason, c.Message})
		}
		return w, nil
	case "statefulset":
		s, err := r.cs.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		w := &Workload{Kind: "StatefulSet", Namespace: ns, Name: name, Desired: 1,
			Ready: s.Status.ReadyReplicas, Updated: s.Status.UpdatedReplicas,
			Generation: s.Generation, ObservedGeneration: s.Status.ObservedGeneration}
		if s.Spec.Replicas != nil {
			w.Desired = *s.Spec.Replicas
		}
		if s.Spec.Selector != nil {
			w.Selector = s.Spec.Selector.MatchLabels
		}
		return w, nil
	case "daemonset":
		d, err := r.cs.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		w := &Workload{Kind: "DaemonSet", Namespace: ns, Name: name, Desired: d.Status.DesiredNumberScheduled,
			Ready: d.Status.NumberReady, Updated: d.Status.UpdatedNumberScheduled,
			Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration}
		if d.Spec.Selector != nil {
			w.Selector = d.Spec.Selector.MatchLabels
		}
		return w, nil
	}
	return nil, fmt.Errorf("unsupported workload kind %q (use deployment, statefulset, daemonset)", kind)
}

// ListPods returns the pods matching the selector. An empty selector is
// refused: it would list every pod in the namespace.
func (r *Reader) ListPods(ctx context.Context, ns string, selector map[string]string) ([]corev1.Pod, error) {
	if err := r.Allowed(ns); err != nil {
		return nil, err
	}
	if len(selector) == 0 {
		return nil, errors.New("refusing to list pods with an empty selector")
	}
	l, err := r.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(selector).String()})
	if err != nil {
		return nil, err
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items, nil
}

// GetPod reads one pod.
func (r *Reader) GetPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	if err := r.Allowed(ns); err != nil {
		return nil, err
	}
	return r.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
}

// GetNode reads one node. Nodes are cluster scoped, so the namespace filter
// does not apply; callers read only nodes that pods in scope run on.
func (r *Reader) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	return r.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

// ListEvents returns events whose involved object has the given name, oldest
// first.
func (r *Reader) ListEvents(ctx context.Context, ns, involvedName string) ([]corev1.Event, error) {
	if err := r.Allowed(ns); err != nil {
		return nil, err
	}
	l, err := r.cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + involvedName})
	if err != nil {
		return nil, err
	}
	sort.Slice(l.Items, func(i, j int) bool { return eventTime(l.Items[i]).Before(eventTime(l.Items[j])) })
	return l.Items, nil
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// PodLogs returns the tail of a container log, bounded in lines and bytes.
func (r *Reader) PodLogs(ctx context.Context, ns, pod, container string, previous bool, tailLines int64) (string, error) {
	if err := r.Allowed(ns); err != nil {
		return "", err
	}
	req := r.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: previous, TailLines: &tailLines, LimitBytes: ptr(int64(maxLogBytes)),
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxLogBytes))
	return string(b), err
}

// Revision is one ReplicaSet generation of a Deployment.
type Revision struct {
	Name     string
	Revision int
	Images   []string
	Created  metav1.Time
	Replicas int32
	Ready    int32
}

// RolloutHistory lists the ReplicaSets owned by a Deployment, newest revision
// first.
func (r *Reader) RolloutHistory(ctx context.Context, ns, deployment string) ([]Revision, error) {
	if err := r.Allowed(ns); err != nil {
		return nil, err
	}
	d, err := r.cs.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels) == 0 {
		return nil, errors.New("deployment has no matchLabels selector")
	}
	l, err := r.cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(d.Spec.Selector.MatchLabels).String()})
	if err != nil {
		return nil, err
	}
	var out []Revision
	for _, rs := range l.Items {
		if !ownedBy(rs.OwnerReferences, d) {
			continue
		}
		rev, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
		v := Revision{Name: rs.Name, Revision: rev, Created: rs.CreationTimestamp, Ready: rs.Status.ReadyReplicas, Replicas: rs.Status.Replicas}
		for _, c := range rs.Spec.Template.Spec.Containers {
			v.Images = append(v.Images, c.Name+"="+c.Image)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out, nil
}

func ownedBy(refs []metav1.OwnerReference, d *appsv1.Deployment) bool {
	for _, o := range refs {
		if o.Kind == "Deployment" && o.Name == d.Name && (o.UID == d.UID || d.UID == "") {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
