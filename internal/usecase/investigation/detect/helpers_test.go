package detect

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// want is the part of a Finding a table test asserts on.
type want struct {
	resource string
	evidence []string
	summary  string // substring
	quote    string // substring
}

func testPod(name string, mut ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "app:1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	for _, m := range mut {
		m(p)
	}
	return p
}

func withMemLimit(limit string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}
	}
}

// withState replaces the first container's current and last state.
func withState(cur, last corev1.ContainerState, restarts int32) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		cs := &p.Status.ContainerStatuses[0]
		cs.State, cs.LastTerminationState, cs.RestartCount, cs.Ready = cur, last, restarts, false
	}
}

func waiting(reason, msg string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
}

func terminated(reason string, exit int32) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: exit}}
}

func podObs(id string, p *corev1.Pod) PodObs { return PodObs{Pod: p, EvidenceID: id} }

func snap(pods ...PodObs) Snapshot { return Snapshot{Pods: pods} }

func assertFindings(t *testing.T, detector string, got []domain.Finding, wants []want) {
	t.Helper()
	if len(got) != len(wants) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(wants), got)
	}
	for i, w := range wants {
		g := got[i]
		if g.Detector != detector {
			t.Errorf("[%d] detector = %q, want %q", i, g.Detector, detector)
		}
		if g.Resource != w.resource {
			t.Errorf("[%d] resource = %q, want %q", i, g.Resource, w.resource)
		}
		if strings.Join(g.EvidenceIDs, ",") != strings.Join(w.evidence, ",") {
			t.Errorf("[%d] evidence = %v, want %v", i, g.EvidenceIDs, w.evidence)
		}
		if len(g.EvidenceIDs) == 0 {
			t.Errorf("[%d] finding cites no evidence", i)
		}
		if !strings.Contains(g.Summary, w.summary) {
			t.Errorf("[%d] summary %q lacks %q", i, g.Summary, w.summary)
		}
		if !strings.Contains(g.Quote, w.quote) {
			t.Errorf("[%d] quote %q lacks %q", i, g.Quote, w.quote)
		}
	}
}
