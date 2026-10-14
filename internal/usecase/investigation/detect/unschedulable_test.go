package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestUnschedulable(t *testing.T) {
	msg := "0/3 nodes are available: 3 Insufficient memory."
	pending := func(status corev1.ConditionStatus, reason string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.NodeName = ""
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = nil
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: status, Reason: reason, Message: msg}}
		}
	}
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy pod", snap(podObs("E2", testPod("api-1"))), nil},
		{"pending unschedulable", snap(podObs("E2", testPod("api-1", pending(corev1.ConditionFalse, corev1.PodReasonUnschedulable)))),
			[]want{{"pod/api-1", []string{"E2"}, "cannot be scheduled", msg}}},
		{"pending other reason", snap(podObs("E2", testPod("api-1", pending(corev1.ConditionFalse, "SchedulingGated")))), nil},
		{"scheduled true", snap(podObs("E2", testPod("api-1", pending(corev1.ConditionTrue, corev1.PodReasonUnschedulable)))), nil},
		{"running with stale condition", snap(podObs("E2", testPod("api-1", pending(corev1.ConditionFalse, corev1.PodReasonUnschedulable),
			func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }))), nil},
		{"no evidence id", snap(podObs("", testPod("api-1", pending(corev1.ConditionFalse, corev1.PodReasonUnschedulable)))), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "unschedulable", unschedulable(tt.s), tt.want)
		})
	}
}
