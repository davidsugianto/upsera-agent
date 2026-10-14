// Package detect holds the deterministic failure detectors. Detectors are pure
// functions over a Snapshot: they never call the cluster or a model, and every
// finding cites the evidence rows it was derived from.
package detect

import (
	corev1 "k8s.io/api/core/v1"

	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

// Snapshot is everything diagnose collected for one workload. Each observation
// carries the ID of the stored evidence row (E1, E2, ...) for the tool call
// that produced it. An empty EvidenceID means the call failed or was skipped;
// detectors must not report from an observation without an EvidenceID.
type Snapshot struct {
	Workload          *kube.Workload
	WorkloadEvidence  string
	Pods              []PodObs
	Nodes             []NodeObs
	Revisions         []kube.Revision
	RevisionsEvidence string
}

// PodObs is one pod of the workload.
type PodObs struct {
	Pod            *corev1.Pod
	EvidenceID     string // k8s_describe
	Events         []corev1.Event
	EventsEvidence string // k8s_events
	// Logs is the redacted tail of the previous container instance when the
	// container has restarted, otherwise of the current one.
	Logs         string
	LogsEvidence string // pod_logs
}

// NodeObs is a node that one of the pods runs on.
type NodeObs struct {
	Node       *corev1.Node
	EvidenceID string // k8s_get kind=node
}
