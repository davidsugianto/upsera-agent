package detect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNode(name string, conds ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Conditions: conds}}
}

func cond(t corev1.NodeConditionType, s corev1.ConditionStatus, reason, msg string) corev1.NodeCondition {
	return corev1.NodeCondition{Type: t, Status: s, Reason: reason, Message: msg}
}

func TestNodeNotReady(t *testing.T) {
	ready := cond(corev1.NodeReady, corev1.ConditionTrue, "KubeletReady", "kubelet is posting ready status")
	pods := []PodObs{podObs("E2", testPod("api-1"))}
	with := func(id string, n *corev1.Node, ps ...PodObs) Snapshot {
		return Snapshot{Pods: ps, Nodes: []NodeObs{{Node: n, EvidenceID: id}}}
	}
	tests := []struct {
		name string
		s    Snapshot
		want []want
	}{
		{"healthy node", with("E5", testNode("node-1", ready), pods...), nil},
		{"ready false", with("E5", testNode("node-1", cond(corev1.NodeReady, corev1.ConditionFalse, "KubeletNotReady", "container runtime is down")), pods...),
			[]want{{"node/node-1", []string{"E2", "E5"}, "not Ready", "container runtime is down"}}},
		{"ready unknown", with("E5", testNode("node-1", cond(corev1.NodeReady, corev1.ConditionUnknown, "NodeStatusUnknown", "Kubelet stopped posting node status.")), pods...),
			[]want{{"node/node-1", []string{"E2", "E5"}, "not Ready", "Kubelet stopped posting"}}},
		{"memory pressure", with("E5", testNode("node-1", ready, cond(corev1.NodeMemoryPressure, corev1.ConditionTrue, "KubeletHasInsufficientMemory", "low memory")), pods...),
			[]want{{"node/node-1", []string{"E2", "E5"}, "MemoryPressure", "low memory"}}},
		{"disk pressure", with("E5", testNode("node-1", ready, cond(corev1.NodeDiskPressure, corev1.ConditionTrue, "KubeletHasDiskPressure", "no space")), pods...),
			[]want{{"node/node-1", []string{"E2", "E5"}, "DiskPressure", "no space"}}},
		{"pid pressure", with("E5", testNode("node-1", ready, cond(corev1.NodePIDPressure, corev1.ConditionTrue, "KubeletHasInsufficientPID", "no pids")), pods...),
			[]want{{"node/node-1", []string{"E2", "E5"}, "PIDPressure", "no pids"}}},
		{"pressure false", with("E5", testNode("node-1", ready, cond(corev1.NodeMemoryPressure, corev1.ConditionFalse, "", "")), pods...), nil},
		{"no pod on the node", with("E5", testNode("node-9", cond(corev1.NodeReady, corev1.ConditionFalse, "", "down")), pods...), nil},
		{"no node evidence id", with("", testNode("node-1", cond(corev1.NodeReady, corev1.ConditionFalse, "", "down")), pods...), nil},
		{"pod without describe evidence", with("E5", testNode("node-1", cond(corev1.NodeReady, corev1.ConditionFalse, "", "down")), podObs("", testPod("api-1"))),
			[]want{{"node/node-1", []string{"E5"}, "not Ready", "down"}}},
		{"nil node", Snapshot{Pods: pods, Nodes: []NodeObs{{EvidenceID: "E5"}}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFindings(t, "node_not_ready", nodeNotReady(tt.s), tt.want)
		})
	}
}
