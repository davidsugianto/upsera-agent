package detect

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestRunOOMKilledPod(t *testing.T) {
	p := testPod("memory-hog-abc", withMemLimit("512Mi"),
		withState(waiting("CrashLoopBackOff", "back-off 40s restarting failed container=app"), terminated("OOMKilled", 137), 4))
	got := Run(snap(podObs("E2", p)))

	var names []string
	for _, f := range got {
		names = append(names, f.Detector)
		if f.Resource != "pod/memory-hog-abc" || !reflect.DeepEqual(f.EvidenceIDs, []string{"E2"}) {
			t.Errorf("%s: resource %q evidence %v, want pod/memory-hog-abc citing E2", f.Detector, f.Resource, f.EvidenceIDs)
		}
	}
	if want := []string{"crashloop", "oomkilled"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("detectors = %v, want %v", names, want)
	}
}

func TestRunHealthySnapshot(t *testing.T) {
	if got := Run(snap(podObs("E2", testPod("api-1")))); len(got) != 0 {
		t.Fatalf("healthy pod produced findings: %+v", got)
	}
	if got := Run(Snapshot{}); len(got) != 0 {
		t.Fatalf("empty snapshot produced findings: %+v", got)
	}
}

func TestRunOrdersByDetectorThenResource(t *testing.T) {
	pull := func(p *corev1.Pod) { withState(waiting("ImagePullBackOff", "x"), corev1.ContainerState{}, 0)(p) }
	oom := withState(terminated("OOMKilled", 137), corev1.ContainerState{}, 0)
	s := snap(
		podObs("E4", testPod("b-pod", pull)),
		podObs("E3", testPod("z-pod", oom)),
		podObs("E2", testPod("a-pod", pull)),
		podObs("E5", testPod("a-pod2", oom)),
	)
	var got []string
	for _, f := range Run(s) {
		got = append(got, f.Detector+" "+f.Resource)
	}
	want := []string{"oomkilled pod/a-pod2", "oomkilled pod/z-pod", "image_pull pod/a-pod", "image_pull pod/b-pod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := 0; i < 20; i++ {
		var again []string
		for _, f := range Run(s) {
			again = append(again, f.Detector+" "+f.Resource)
		}
		if !reflect.DeepEqual(again, want) {
			t.Fatalf("non-deterministic order: %v", again)
		}
	}
}
