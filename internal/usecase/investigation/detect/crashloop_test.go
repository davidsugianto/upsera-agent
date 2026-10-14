package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestCrashloop(t *testing.T) {
	initBackoff := func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "migrate", RestartCount: 4, State: waiting("CrashLoopBackOff", "back-off 40s restarting failed container"),
		}}
	}
	initDone := func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "migrate", RestartCount: 3, State: terminated("Completed", 0),
		}}
	}
	initFailed := func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "migrate", RestartCount: 3, State: terminated("Error", 1),
		}}
	}
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy pod", snap(podObs("E2", testPod("api-1"))), nil},
		{"waiting CrashLoopBackOff", snap(podObs("E2", testPod("api-1",
			withState(waiting("CrashLoopBackOff", "back-off 5m0s restarting failed container=app"), terminated("Error", 1), 6)))),
			[]want{{"pod/api-1", []string{"E2"}, `"app"`, "CrashLoopBackOff"}}},
		{"restarts>=3 and terminated", snap(podObs("E2", testPod("api-1", withState(terminated("Error", 2), corev1.ContainerState{}, 3)))),
			[]want{{"pod/api-1", []string{"E2"}, "restarted 3 times", "Error"}}},
		{"restarts<3 and terminated", snap(podObs("E2", testPod("api-1", withState(terminated("Error", 2), corev1.ContainerState{}, 2)))), nil},
		{"many restarts but running", snap(podObs("E2", testPod("api-1", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 9 }))), nil},
		{"init container backoff", snap(podObs("E2", testPod("api-1", initBackoff))),
			[]want{{"pod/api-1", []string{"E2"}, `init container "migrate"`, "CrashLoopBackOff"}}},
		{"init container completed after retries", snap(podObs("E2", testPod("api-1", initDone))), nil},
		{"init container failed after retries", snap(podObs("E2", testPod("api-1", initFailed))),
			[]want{{"pod/api-1", []string{"E2"}, `init container "migrate" restarted 3 times`, "Error"}}},
		{"no evidence id", snap(podObs("", testPod("api-1", withState(waiting("CrashLoopBackOff", ""), corev1.ContainerState{}, 6)))), nil},
		{"nil pod", snap(PodObs{EvidenceID: "E2"}), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "crashloop", crashloop(tt.s), tt.want)
		})
	}
}
