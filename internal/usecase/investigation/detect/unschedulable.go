package detect

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// unschedulable reports Pending pods the scheduler has rejected, quoting its
// message. The phase check keeps a stale PodScheduled=False condition on a pod
// that has since started from being reported.
func unschedulable(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, o := range s.Pods {
		if !o.usable() || o.Pod.Status.Phase != corev1.PodPending {
			continue
		}
		for _, c := range o.Pod.Status.Conditions {
			if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable {
				continue
			}
			out = append(out, domain.Finding{
				Detector:    detUnschedulable,
				Resource:    podResource(o.Pod),
				Summary:     "pod is Pending and cannot be scheduled (Unschedulable)",
				Quote:       firstNonEmpty(c.Message, c.Reason),
				EvidenceIDs: evidenceIDs(o.EvidenceID),
			})
			break
		}
	}
	return out
}
