package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestEvicted(t *testing.T) {
	msg := "The node was low on resource: memory."
	failed := func(reason string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodFailed, reason, msg
		}
	}
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy pod", snap(podObs("E2", testPod("api-1"))), nil},
		{"evicted", snap(podObs("E2", testPod("api-1", failed("Evicted")))),
			[]want{{"pod/api-1", []string{"E2"}, "evicted", msg}}},
		{"failed other reason", snap(podObs("E2", testPod("api-1", failed("DeadlineExceeded")))), nil},
		{"evicted reason but running", snap(podObs("E2", testPod("api-1", failed("Evicted"), func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }))), nil},
		{"no evidence id", snap(podObs("", testPod("api-1", failed("Evicted")))), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "evicted", evicted(tt.s), tt.want)
		})
	}
}
