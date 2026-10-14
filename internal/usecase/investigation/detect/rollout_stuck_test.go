package detect

import (
	"testing"

	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

func TestRolloutStuck(t *testing.T) {
	deploy := func(mut func(*kube.Workload)) *kube.Workload {
		w := &kube.Workload{Kind: "Deployment", Namespace: "shop", Name: "api", Desired: 3, Ready: 3, Updated: 3, Generation: 4, ObservedGeneration: 4}
		if mut != nil {
			mut(w)
		}
		return w
	}
	snapW := func(w *kube.Workload, wid, rid string) Snapshot {
		s := Snapshot{Workload: w, WorkloadEvidence: wid, RevisionsEvidence: rid}
		if rid != "" {
			s.Revisions = []kube.Revision{{Name: "api-7d9", Revision: 5, Images: []string{"api:2"}}, {Name: "api-6c8", Revision: 4, Images: []string{"api:1"}}}
		}
		return s
	}
	deadline := func(w *kube.Workload) {
		w.Conditions = []kube.Condition{{Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded", Message: `ReplicaSet "api-7d9" has timed out progressing.`}}
	}
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy deployment", snapW(deploy(nil), "E1", "E9"), nil},
		{"progress deadline exceeded", snapW(deploy(deadline), "E1", "E9"),
			[]want{{"deployment/api", []string{"E1", "E9"}, "ProgressDeadlineExceeded", "has timed out progressing"}}},
		{"cites revisions only when present", snapW(deploy(deadline), "E1", ""),
			[]want{{"deployment/api", []string{"E1"}, "ProgressDeadlineExceeded", "has timed out progressing"}}},
		{"summary names latest revision", snapW(deploy(deadline), "E1", "E9"),
			[]want{{"deployment/api", []string{"E1", "E9"}, "revision 5 (api:2)", ""}}},
		{"progressing true", snapW(deploy(func(w *kube.Workload) {
			w.Conditions = []kube.Condition{{Type: "Progressing", Status: "True", Reason: "NewReplicaSetAvailable"}}
		}), "E1", ""), nil},
		{"generation not observed", snapW(deploy(func(w *kube.Workload) { w.ObservedGeneration = 3 }), "E1", ""),
			[]want{{"deployment/api", []string{"E1"}, "observed generation 3 < generation 4", "generation 4"}}},
		{"updated and ready below desired", snapW(deploy(func(w *kube.Workload) { w.Updated, w.Ready = 1, 2 }), "E1", ""),
			[]want{{"deployment/api", []string{"E1"}, "1 updated", "desired 3"}}},
		{"updated below desired but ready", snapW(deploy(func(w *kube.Workload) { w.Updated = 1 }), "E1", ""), nil},
		{"ready below desired but updated", snapW(deploy(func(w *kube.Workload) { w.Ready = 1 }), "E1", ""), nil},
		{"statefulset ignored", snapW(deploy(func(w *kube.Workload) { w.Kind = "StatefulSet"; deadline(w) }), "E1", ""), nil},
		{"no workload evidence id", snapW(deploy(deadline), "", "E9"), nil},
		{"nil workload", Snapshot{WorkloadEvidence: "E1"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "rollout_stuck", rolloutStuck(tt.s), tt.want)
		})
	}
}
