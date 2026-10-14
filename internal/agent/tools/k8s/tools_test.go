package k8stools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	"github.com/davidsugianto/upsera-agent/internal/config"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

const (
	secretValue = "hunter2-do-not-leak"
	logToken    = "tok_9f8e7d6c5b4a3921"
)

func labels() map[string]string { return map[string]string{"app": "api"} }

func fixtureObjects() []runtime.Object {
	three := int32(3)
	oneAgo := metav1.NewTime(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-abc", Namespace: "shop", Labels: labels()},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{
				Name: "app", Image: "shop/api:1.2.3",
				Env: []corev1.EnvVar{
					{Name: "DB_PASSWORD", Value: secretValue},
					{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "api-secrets"}, Key: "key"}}},
				},
				EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "api-config"}}}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: mem},
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
					PeriodSeconds: 10, FailureThreshold: 3,
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: false, RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137, FinishedAt: oneAgo}},
			}},
		},
	}
	other := pod.DeepCopy()
	other.Name = "api-def"
	other.Status.ContainerStatuses[0].Ready = true
	other.Status.ContainerStatuses[0].RestartCount = 0
	other.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: oneAgo}}
	other.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}

	return []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Generation: 4},
			Spec:       appsv1.DeploymentSpec{Replicas: &three, Selector: &metav1.LabelSelector{MatchLabels: labels()}},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 2, ObservedGeneration: 4,
				Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "api-7d9", Namespace: "shop", Labels: labels(),
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}}},
			Spec:   appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "shop/api:1.2.3"}}}}},
			Status: appsv1.ReplicaSetStatus{Replicas: 2, ReadyReplicas: 1},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, Reason: "NodeStatusUnknown", Message: "Kubelet stopped posting node status."}}},
		},
		pod, other,
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev1", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-abc"},
			Type:           corev1.EventTypeWarning, Reason: "BackOff", Message: "Back-off restarting failed container",
			Count: 12, LastTimestamp: oneAgo,
		},
	}
}

func setup(t *testing.T, deny []string) (map[string]tools.Tool, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(fixtureObjects()...)
	ts := NewTools(kube.NewReader(cs, config.NamespaceFilters{Deny: deny}))
	m := make(map[string]tools.Tool, len(ts))
	for _, tl := range ts {
		m[tl.Name()] = tl
	}
	return m, cs
}

func setLogs(cs *fake.Clientset, body string) {
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: []byte(body)}, nil
	})
}

func call(t *testing.T, ts map[string]tools.Tool, name, args string) (tools.Result, error) {
	t.Helper()
	tl, ok := ts[name]
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	return tl.Call(context.Background(), []byte(args))
}

func mustCall(t *testing.T, ts map[string]tools.Tool, name, args string) tools.Result {
	t.Helper()
	res, err := call(t, ts, name, args)
	if err != nil {
		t.Fatalf("%s(%s): %v", name, args, err)
	}
	return res
}

func TestNewToolsContract(t *testing.T) {
	ts, _ := setup(t, nil)
	want := []string{NameGet, NameListPods, NameDescribe, NameEvents, NamePodLogs, NameRolloutHistory}
	if len(ts) != len(want) {
		t.Fatalf("got %d tools, want %d", len(ts), len(want))
	}
	for _, n := range want {
		tl, ok := ts[n]
		if !ok {
			t.Fatalf("missing tool %q", n)
		}
		if tl.Mutating() {
			t.Errorf("%s reports Mutating() == true", n)
		}
		if tl.Description() == "" {
			t.Errorf("%s has no description", n)
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
			t.Errorf("%s schema is not valid JSON: %v", n, err)
		} else if schema["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", n, schema["type"])
		}
	}
	// A registry must keep all of them even with remediation disabled.
	var all []tools.Tool
	for _, tl := range ts {
		all = append(all, tl)
	}
	if got := len(tools.NewRegistry(false, all...).Names()); got != len(want) {
		t.Errorf("registry(false) kept %d tools, want %d", got, len(want))
	}
}

func TestGetWorkload(t *testing.T) {
	ts, _ := setup(t, nil)
	res := mustCall(t, ts, NameGet, `{"kind":"deploy","namespace":"shop","name":"api"}`)
	w, ok := res.Data.(*kube.Workload)
	if !ok {
		t.Fatalf("Data is %T, want *kube.Workload", res.Data)
	}
	if w.Name != "api" || w.Desired != 3 || w.Selector["app"] != "api" {
		t.Errorf("workload = %+v", w)
	}
	for _, s := range []string{"Deployment", "shop/api", "ProgressDeadlineExceeded"} {
		if !strings.Contains(res.Summary, s) {
			t.Errorf("summary missing %q:\n%s", s, res.Summary)
		}
	}
}

func TestGetNode(t *testing.T) {
	ts, _ := setup(t, []string{"kube-system"})
	res := mustCall(t, ts, NameGet, `{"kind":"node","name":"node-1"}`)
	if _, ok := res.Data.(*corev1.Node); !ok {
		t.Fatalf("Data is %T, want *corev1.Node", res.Data)
	}
	for _, s := range []string{"node-1", "Ready=Unknown", "NodeStatusUnknown", "node.kubernetes.io/unreachable"} {
		if !strings.Contains(res.Summary, s) {
			t.Errorf("summary missing %q:\n%s", s, res.Summary)
		}
	}
}

func TestListPods(t *testing.T) {
	ts, _ := setup(t, nil)
	res := mustCall(t, ts, NameListPods, `{"namespace":"shop","selector":{"app":"api"}}`)
	pods, ok := res.Data.([]corev1.Pod)
	if !ok || len(pods) != 2 {
		t.Fatalf("Data = %T len %d, want 2 pods", res.Data, len(pods))
	}
	lines := strings.Split(strings.TrimSpace(res.Summary), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per pod, got:\n%s", res.Summary)
	}
	for _, s := range []string{"api-abc", "Running", "0/1", "restarts=7", "node-1"} {
		if !strings.Contains(lines[0], s) {
			t.Errorf("first line missing %q: %s", s, lines[0])
		}
	}
	if !strings.Contains(lines[1], "1/1") {
		t.Errorf("second line should be ready 1/1: %s", lines[1])
	}
}

func TestListPodsNoMatch(t *testing.T) {
	ts, _ := setup(t, nil)
	res := mustCall(t, ts, NameListPods, `{"namespace":"shop","selector":{"app":"ghost"}}`)
	if pods, _ := res.Data.([]corev1.Pod); len(pods) != 0 {
		t.Errorf("want no pods, got %d", len(pods))
	}
	if res.Summary == "" {
		t.Error("an empty result should still say so")
	}
}

func TestDescribeNeverPrintsEnvValues(t *testing.T) {
	ts, _ := setup(t, nil)
	res := mustCall(t, ts, NameDescribe, `{"namespace":"shop","pod":"api-abc"}`)
	if _, ok := res.Data.(*corev1.Pod); !ok {
		t.Fatalf("Data is %T, want *corev1.Pod", res.Data)
	}
	if strings.Contains(res.Summary, secretValue) {
		t.Fatalf("env value leaked into summary:\n%s", res.Summary)
	}
	for _, s := range []string{
		"api-abc", "shop", "node-1", "Running",
		"DB_PASSWORD", "API_KEY", "api-config",
		"shop/api:1.2.3", "restartCount=7", "CrashLoopBackOff",
		"OOMKilled", "137", "2026-10-14T09:00:00Z",
		"cpu=250m", "memory=128Mi",
		"ContainersNotReady", "liveness", "/healthz",
	} {
		if !strings.Contains(res.Summary, s) {
			t.Errorf("summary missing %q:\n%s", s, res.Summary)
		}
	}
}

func TestEvents(t *testing.T) {
	ts, _ := setup(t, nil)
	res := mustCall(t, ts, NameEvents, `{"namespace":"shop","name":"api-abc"}`)
	evs, ok := res.Data.([]corev1.Event)
	if !ok || len(evs) != 1 {
		t.Fatalf("Data = %T len %d, want 1 event", res.Data, len(evs))
	}
	for _, s := range []string{"Warning", "BackOff", "Back-off restarting failed container", "x12"} {
		if !strings.Contains(res.Summary, s) {
			t.Errorf("summary missing %q:\n%s", s, res.Summary)
		}
	}
}

func TestEventsKeepsNewestWhenTrimmed(t *testing.T) {
	cs := fake.NewClientset()
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i := range maxEvents + 10 {
		_, err := cs.CoreV1().Events("shop").Create(context.Background(), &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("e%03d", i), Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "p"},
			Reason:         fmt.Sprintf("Reason%03d", i), Type: corev1.EventTypeNormal,
			LastTimestamp: metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	ts := NewTools(kube.NewReader(cs, config.NamespaceFilters{}))
	res, err := ts[3].Call(context.Background(), []byte(`{"namespace":"shop","name":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("dropping old events must set Truncated")
	}
	last := fmt.Sprintf("Reason%03d", maxEvents+9)
	if !strings.Contains(res.Summary, last) {
		t.Errorf("newest event %s missing", last)
	}
	if strings.Contains(res.Summary, "Reason000") {
		t.Error("oldest event should have been dropped")
	}
	if evs := res.Data.([]corev1.Event); len(evs) != maxEvents+10 {
		t.Errorf("Data should hold every event, got %d", len(evs))
	}
}

func TestPodLogsRedactsAndDefaultsTail(t *testing.T) {
	ts, cs := setup(t, nil)
	setLogs(cs, "starting up\nconnecting with token="+logToken+" ok\nAuthorization: Bearer abcdef1234567890\ndone\n")
	res := mustCall(t, ts, NamePodLogs, `{"namespace":"shop","pod":"api-abc"}`)

	body, ok := res.Data.(string)
	if !ok {
		t.Fatalf("Data is %T, want string", res.Data)
	}
	for _, s := range []string{res.Summary, body} {
		if strings.Contains(s, logToken) || strings.Contains(s, "abcdef1234567890") {
			t.Fatalf("secret leaked:\n%s", s)
		}
		if !strings.Contains(s, "starting up") || !strings.Contains(s, "done") {
			t.Errorf("log text lost:\n%s", s)
		}
	}
	if len(res.Redactions) == 0 {
		t.Error("Redactions should record what was removed")
	}
	if got := lastLogOptions(t, cs).TailLines; got == nil || *got != 100 {
		t.Errorf("default tail = %v, want 100", got)
	}
}

func lastLogOptions(t *testing.T, cs *fake.Clientset) *corev1.PodLogOptions {
	t.Helper()
	var opts *corev1.PodLogOptions
	for _, a := range cs.Actions() {
		if a.GetSubresource() == "log" {
			opts = a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		}
	}
	if opts == nil {
		t.Fatal("no log request recorded")
	}
	return opts
}

func TestPodLogsTailLinesAndPrevious(t *testing.T) {
	ts, cs := setup(t, nil)
	mustCall(t, ts, NamePodLogs, `{"namespace":"shop","pod":"api-abc","container":"app","previous":true,"tail_lines":50}`)
	opts := lastLogOptions(t, cs)
	if *opts.TailLines != 50 || !opts.Previous || opts.Container != "app" {
		t.Errorf("options = %+v", opts)
	}
	mustCall(t, ts, NamePodLogs, `{"namespace":"shop","pod":"api-abc","tail_lines":5000}`)
	if got := *lastLogOptions(t, cs).TailLines; got != 200 {
		t.Errorf("tail_lines 5000 clamped to %d, want 200", got)
	}
}

func TestPodLogsTruncatesKeepingTail(t *testing.T) {
	ts, cs := setup(t, nil)
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "line-%03d %s\n", i, strings.Repeat("x", 90))
	}
	setLogs(cs, b.String())
	res := mustCall(t, ts, NamePodLogs, `{"namespace":"shop","pod":"api-abc"}`)
	if !res.Truncated {
		t.Fatal("Truncated should be set")
	}
	if len(res.Summary) > maxSummaryBytes+128 {
		t.Errorf("summary is %d bytes, cap %d", len(res.Summary), maxSummaryBytes)
	}
	if !strings.Contains(res.Summary, "line-199") {
		t.Error("the end of the log is the useful part and must survive truncation")
	}
	if strings.Contains(res.Summary, "line-000") {
		t.Error("the head should have been cut")
	}
	if body := res.Data.(string); !strings.Contains(body, "line-000") {
		t.Error("Data keeps the whole redacted tail")
	}
}

func TestRolloutHistory(t *testing.T) {
	ts, _ := setup(t, nil)
	res := mustCall(t, ts, NameRolloutHistory, `{"namespace":"shop","name":"api"}`)
	revs, ok := res.Data.([]kube.Revision)
	if !ok || len(revs) != 1 {
		t.Fatalf("Data = %T len %d, want 1 revision", res.Data, len(revs))
	}
	for _, s := range []string{"api-7d9", "2", "app=shop/api:1.2.3"} {
		if !strings.Contains(res.Summary, s) {
			t.Errorf("summary missing %q:\n%s", s, res.Summary)
		}
	}
}

func TestSummaryIsRedacted(t *testing.T) {
	// A secret-shaped string in an event message must not survive into Summary.
	cs := fake.NewClientset(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Name: "p"},
		Type:           corev1.EventTypeWarning, Reason: "Failed",
		Message: "pull failed password=" + secretValue,
	})
	ts := NewTools(kube.NewReader(cs, config.NamespaceFilters{}))
	res, err := ts[3].Call(context.Background(), []byte(`{"namespace":"shop","name":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Summary, secretValue) {
		t.Errorf("secret leaked:\n%s", res.Summary)
	}
	if res.Redactions["secret_pair"] == 0 {
		t.Errorf("Redactions = %v, want a secret_pair count", res.Redactions)
	}
}

func TestBadArgs(t *testing.T) {
	ts, cs := setup(t, nil)
	cases := []struct{ tool, args string }{
		{NameGet, `{"kind":"deployment","namespace":"shop"}`},                      // name missing
		{NameGet, `{"namespace":"shop","name":"api"}`},                             // kind missing
		{NameGet, `{"kind":"deployment","name":"api"}`},                            // namespace missing for a workload
		{NameGet, `{"kind":"pod","namespace":"shop","name":"api-abc"}`},            // unsupported kind
		{NameGet, `{"kind":"node","name":"node-1","extra":1}`},                     // unknown field
		{NameListPods, `{"namespace":"shop"}`},                                     // selector missing
		{NameListPods, `{"namespace":"shop","selector":{}}`},                       // selector empty
		{NameListPods, `{"selector":{"app":"api"}}`},                               // namespace missing
		{NameListPods, `{"namespace":"shop","selector":{"app":"api"},"x":true}`},   // unknown field
		{NameDescribe, `{"namespace":"shop"}`},                                     // pod missing
		{NameDescribe, `{"namespace":" ","pod":"api-abc"}`},                        // blank namespace
		{NameDescribe, `{"namespace":"shop","pod":"api-abc","env":true}`},          // unknown field
		{NameEvents, `{"namespace":"shop"}`},                                       // name missing
		{NameEvents, `{"name":"api-abc"}`},                                         // namespace missing
		{NamePodLogs, `{"namespace":"shop"}`},                                      // pod missing
		{NamePodLogs, `{"namespace":"shop","pod":"api-abc","tail_lines":-1}`},      // negative tail
		{NamePodLogs, `{"namespace":"shop","pod":"api-abc","tail_lines":"ten"}`},   // wrong type
		{NamePodLogs, `{"namespace":"shop","pod":"api-abc","follow":true}`},        // unknown field
		{NameRolloutHistory, `{"namespace":"shop"}`},                               // name missing
		{NameRolloutHistory, `{"namespace":"shop","name":"api","revision":2}`},     // unknown field
		{NameRolloutHistory, `not json`},                                           // malformed
		{NameRolloutHistory, ``},                                                   // empty
		{NameRolloutHistory, `{"namespace":"shop","name":"api"} {"name":"other"}`}, // trailing data
	}
	for _, c := range cases {
		_, err := call(t, ts, c.tool, c.args)
		if err == nil {
			t.Errorf("%s(%s): expected error", c.tool, c.args)
			continue
		}
		if !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s(%s): error should wrap ErrInvalidArgs: %v", c.tool, c.args, err)
		}
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("invalid args reached the API %d times", n)
	}
}

func TestDeniedNamespace(t *testing.T) {
	ts, cs := setup(t, []string{"shop"})
	cases := []struct{ tool, args string }{
		{NameGet, `{"kind":"deployment","namespace":"shop","name":"api"}`},
		{NameListPods, `{"namespace":"shop","selector":{"app":"api"}}`},
		{NameDescribe, `{"namespace":"shop","pod":"api-abc"}`},
		{NameEvents, `{"namespace":"shop","name":"api-abc"}`},
		{NamePodLogs, `{"namespace":"shop","pod":"api-abc"}`},
		{NameRolloutHistory, `{"namespace":"shop","name":"api"}`},
	}
	for _, c := range cases {
		_, err := call(t, ts, c.tool, c.args)
		if !errors.Is(err, kube.ErrNamespaceDenied) {
			t.Errorf("%s: want ErrNamespaceDenied, got %v", c.tool, err)
		}
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("denied namespace reached the API %d times", n)
	}
}

func TestNotFoundIsWrapped(t *testing.T) {
	ts, _ := setup(t, nil)
	_, err := call(t, ts, NameDescribe, `{"namespace":"shop","pod":"ghost"}`)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want a not-found error naming the pod, got %v", err)
	}
	if !strings.Contains(err.Error(), NameDescribe) {
		t.Errorf("error should name the tool: %v", err)
	}
}

func TestOnlyReadVerbsReachTheAPI(t *testing.T) {
	ts, cs := setup(t, nil)
	for name, args := range map[string]string{
		NameGet:            `{"kind":"deployment","namespace":"shop","name":"api"}`,
		NameListPods:       `{"namespace":"shop","selector":{"app":"api"}}`,
		NameDescribe:       `{"namespace":"shop","pod":"api-abc"}`,
		NameEvents:         `{"namespace":"shop","name":"api-abc"}`,
		NamePodLogs:        `{"namespace":"shop","pod":"api-abc"}`,
		NameRolloutHistory: `{"namespace":"shop","name":"api"}`,
	} {
		mustCall(t, ts, name, args)
	}
	mustCall(t, ts, NameGet, `{"kind":"node","name":"node-1"}`)
	for _, a := range cs.Actions() {
		if v := a.GetVerb(); v != "get" && v != "list" {
			t.Errorf("mutating verb %q on %s", v, a.GetResource().Resource)
		}
	}
}

func TestDecodeKeepsJSONErrorReachable(t *testing.T) {
	var v struct{}
	err := decode([]byte(`{not json`), &v)
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("error should wrap ErrInvalidArgs: %v", err)
	}
	var syn *json.SyntaxError
	if !errors.As(err, &syn) {
		t.Fatalf("error should still expose *json.SyntaxError: %v", err)
	}
}

// Evidence excerpts are built from Summary, so invisible characters that split
// a secret must not defeat redaction there either.
func TestFinishRedactedHandlesSplitSecrets(t *testing.T) {
	res, full := finishRedacted("ok\npass\x00word=hunter2\nbearer \u200bsecrettoken123\n", true)
	for _, text := range []string{res.Summary, full} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "secrettoken123") {
			t.Fatalf("secret survived: %q", text)
		}
		if !strings.Contains(text, "\n") {
			t.Fatalf("newlines were lost: %q", text)
		}
	}
}
