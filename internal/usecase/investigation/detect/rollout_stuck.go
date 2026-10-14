package detect

import (
	"fmt"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// rolloutStuck reports a Deployment whose rollout is not converging: the
// progress deadline passed, the controller has not observed the latest spec, or
// fewer replicas are updated and ready than desired. Revision history is cited
// only when it was collected.
func rolloutStuck(s Snapshot) []domain.Finding {
	w := s.Workload
	if w == nil || s.WorkloadEvidence == "" || !strings.EqualFold(w.Kind, "deployment") {
		return nil
	}
	var parts []string
	var quote string
	for _, c := range w.Conditions {
		if c.Type == "Progressing" && c.Status == "False" && c.Reason == "ProgressDeadlineExceeded" {
			parts = append(parts, "Progressing=False (ProgressDeadlineExceeded)")
			quote = firstNonEmpty(c.Message, c.Reason)
			break
		}
	}
	if w.ObservedGeneration < w.Generation {
		parts = append(parts, fmt.Sprintf("observed generation %d < generation %d", w.ObservedGeneration, w.Generation))
		quote = firstNonEmpty(quote, fmt.Sprintf("observedGeneration %d < generation %d", w.ObservedGeneration, w.Generation))
	}
	if w.Updated < w.Desired && w.Ready < w.Desired {
		parts = append(parts, fmt.Sprintf("%d updated, %d ready of %d desired", w.Updated, w.Ready, w.Desired))
		quote = firstNonEmpty(quote, fmt.Sprintf("desired %d, updated %d, ready %d", w.Desired, w.Updated, w.Ready))
	}
	if len(parts) == 0 {
		return nil
	}
	ids := []string{s.WorkloadEvidence}
	if s.RevisionsEvidence != "" {
		ids = append(ids, s.RevisionsEvidence)
		if len(s.Revisions) > 0 {
			r := s.Revisions[0]
			parts = append(parts, fmt.Sprintf("latest revision %d (%s)", r.Revision, strings.Join(r.Images, ", ")))
		}
	}
	return []domain.Finding{{
		Detector:    detRolloutStuck,
		Resource:    "deployment/" + w.Name,
		Summary:     "rollout is stuck: " + strings.Join(parts, "; "),
		Quote:       quote,
		EvidenceIDs: evidenceIDs(ids...),
	}}
}
