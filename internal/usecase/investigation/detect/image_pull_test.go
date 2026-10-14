package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestImagePull(t *testing.T) {
	msg := `Back-off pulling image "app:nope"`
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy pod", snap(podObs("E2", testPod("api-1"))), nil},
		{"ImagePullBackOff", snap(podObs("E2", testPod("api-1", withState(waiting("ImagePullBackOff", msg), corev1.ContainerState{}, 0)))),
			[]want{{"pod/api-1", []string{"E2"}, "ImagePullBackOff", msg}}},
		{"ErrImagePull", snap(podObs("E2", testPod("api-1", withState(waiting("ErrImagePull", "rpc error: not found"), corev1.ContainerState{}, 0)))),
			[]want{{"pod/api-1", []string{"E2"}, "ErrImagePull", "rpc error: not found"}}},
		{"other waiting reason", snap(podObs("E2", testPod("api-1", withState(waiting("ContainerCreating", ""), corev1.ContainerState{}, 0)))), nil},
		{"init container", snap(podObs("E2", testPod("api-1", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "migrate", State: waiting("ImagePullBackOff", msg)}}
		}))), []want{{"pod/api-1", []string{"E2"}, `init container "migrate"`, msg}}},
		{"no evidence id", snap(podObs("", testPod("api-1", withState(waiting("ImagePullBackOff", msg), corev1.ContainerState{}, 0)))), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "image_pull", imagePull(tt.s), tt.want)
		})
	}
}
