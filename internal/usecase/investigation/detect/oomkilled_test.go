package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestOOMKilled(t *testing.T) {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy pod", snap(podObs("E2", testPod("api-1"))), nil},
		{"last state OOMKilled", snap(podObs("E2", testPod("api-1", withMemLimit("512Mi"),
			withState(waiting("CrashLoopBackOff", ""), terminated("OOMKilled", 137), 5)))),
			[]want{{"pod/api-1", []string{"E2"}, "memory limit 512Mi", "OOMKilled"}}},
		{"current state OOMKilled", snap(podObs("E2", testPod("api-1", withMemLimit("64Mi"),
			withState(terminated("OOMKilled", 137), corev1.ContainerState{}, 0)))),
			[]want{{"pod/api-1", []string{"E2"}, "memory limit 64Mi", "exit code 137"}}},
		{"exit 137 without reason", snap(podObs("E2", testPod("api-1", withMemLimit("64Mi"),
			withState(running, terminated("Error", 137), 1)))),
			[]want{{"pod/api-1", []string{"E2"}, "memory limit 64Mi", "exit code 137"}}},
		{"no memory limit", snap(podObs("E2", testPod("api-1", withState(running, terminated("OOMKilled", 137), 1)))),
			[]want{{"pod/api-1", []string{"E2"}, "no memory limit", "OOMKilled"}}},
		{"other exit code", snap(podObs("E2", testPod("api-1", withState(running, terminated("Error", 1), 1)))), nil},
		{"init container", snap(podObs("E2", testPod("api-1", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "migrate", State: terminated("OOMKilled", 137)}}
		}))), []want{{"pod/api-1", []string{"E2"}, `init container "migrate"`, "OOMKilled"}}},
		{"no evidence id", snap(podObs("", testPod("api-1", withState(running, terminated("OOMKilled", 137), 1)))), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "oomkilled", oomkilled(tt.s), tt.want)
		})
	}
}
