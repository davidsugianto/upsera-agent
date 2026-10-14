package detect

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// evicted reports pods the kubelet evicted, quoting its message.
func evicted(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, o := range s.Pods {
		if !o.usable() || o.Pod.Status.Phase != corev1.PodFailed || o.Pod.Status.Reason != "Evicted" {
			continue
		}
		out = append(out, domain.Finding{
			Detector:    detEvicted,
			Resource:    podResource(o.Pod),
			Summary:     "pod was evicted by the kubelet",
			Quote:       firstNonEmpty(o.Pod.Status.Message, o.Pod.Status.Reason),
			EvidenceIDs: evidenceIDs(o.EvidenceID),
		})
	}
	return out
}
