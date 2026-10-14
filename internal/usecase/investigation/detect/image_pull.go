package detect

import (
	"fmt"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// imagePull reports containers (init included) that are waiting because the
// image cannot be pulled, quoting the kubelet's message.
func imagePull(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, o := range s.Pods {
		if !o.usable() {
			continue
		}
		var parts []string
		var quote string
		for _, c := range containers(o.Pod) {
			w := c.State.Waiting
			if w == nil || (w.Reason != "ImagePullBackOff" && w.Reason != "ErrImagePull") {
				continue
			}
			image := c.Image
			if sp := c.spec(o.Pod); sp != nil {
				image = sp.Image
			}
			parts = append(parts, fmt.Sprintf("%s cannot pull image %q (%s)", c.label(), image, w.Reason))
			if quote == "" {
				quote = firstNonEmpty(w.Message, w.Reason)
			}
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, domain.Finding{
			Detector:    detImagePull,
			Resource:    podResource(o.Pod),
			Summary:     strings.Join(parts, "; "),
			Quote:       quote,
			EvidenceIDs: evidenceIDs(o.EvidenceID),
		})
	}
	return out
}
