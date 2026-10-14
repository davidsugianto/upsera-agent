package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestProbeFailure(t *testing.T) {
	ev := func(reason, msg string) []corev1.Event {
		return []corev1.Event{{Reason: reason, Message: msg, Type: corev1.EventTypeWarning}}
	}
	notReady := func(p *corev1.Pod) {
		p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{}
		p.Status.ContainerStatuses[0].Ready = false
	}
	withEvents := func(id string, evs []corev1.Event, p *corev1.Pod) PodObs {
		o := podObs("E2", p)
		o.Events, o.EventsEvidence = evs, id
		return o
	}
	readiness := "Readiness probe failed: HTTP probe failed with statuscode: 503"
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy pod", snap(podObs("E2", testPod("api-1"))), nil},
		// A startup blip leaves a readiness event behind after the pod recovers.
		{"readiness event on a pod that is Ready now", snap(withEvents("E3", ev("Unhealthy", readiness), testPod("api-1"))), nil},
		{"readiness event on a pod that is not Ready", snap(withEvents("E3", ev("Unhealthy", readiness), testPod("api-1", notReady))),
			[]want{{"pod/api-1", []string{"E2", "E3"}, "probe", readiness}}},
		{"liveness event", snap(withEvents("E3", ev("Unhealthy", "Liveness probe failed: connection refused"), testPod("api-1"))),
			[]want{{"pod/api-1", []string{"E2", "E3"}, "probe", "Liveness probe failed"}}},
		{"event without events evidence", snap(withEvents("", ev("Unhealthy", readiness), testPod("api-1"))), nil},
		{"unrelated event", snap(withEvents("E3", ev("Pulled", "Readiness probe failed"), testPod("api-1"))), nil},
		{"unhealthy but not a probe", snap(withEvents("E3", ev("Unhealthy", "node unhealthy"), testPod("api-1"))), nil},
		{"running not ready with readiness probe", snap(podObs("E2", testPod("api-1", notReady))),
			[]want{{"pod/api-1", []string{"E2"}, `container "app" is running but not Ready`, "Ready=false"}}},
		{"running not ready without probe", snap(podObs("E2", testPod("api-1", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false }))), nil},
		{"not ready but crashing", snap(podObs("E2", testPod("api-1", notReady, withState(waiting("CrashLoopBackOff", ""), terminated("Error", 1), 4)))), nil},
		{"not ready but another container terminated", snap(podObs("E2", testPod("api-1", notReady, func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "side"})
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: "side", State: terminated("Error", 1)})
		}))), nil},
		{"event and status share one finding", snap(withEvents("E3", ev("Unhealthy", readiness), testPod("api-1", notReady))),
			[]want{{"pod/api-1", []string{"E2", "E3"}, "probe", readiness}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "probe_failure", probeFailure(tt.s), tt.want)
		})
	}
}
