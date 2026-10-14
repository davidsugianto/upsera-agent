package detect

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// oomExitCode is 128+SIGKILL, which the kernel OOM killer produces.
const oomExitCode = 137

// oomkilled reports containers (init included) whose current or last
// termination was an OOM kill, with the memory limit that applied.
func oomkilled(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, o := range s.Pods {
		if !o.usable() {
			continue
		}
		var parts []string
		var quote string
		for _, c := range containers(o.Pod) {
			t := oomTermination(c.State.Terminated)
			if t == nil {
				t = oomTermination(c.LastTerminationState.Terminated)
			}
			if t == nil {
				continue
			}
			q := fmt.Sprintf("%s (exit code %d)", firstNonEmpty(t.Reason, "terminated"), t.ExitCode)
			parts = append(parts, fmt.Sprintf("%s terminated with %s; %s", c.label(), q, memoryLimit(o.Pod, c)))
			if quote == "" {
				quote = q
			}
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, domain.Finding{
			Detector:    detOOMKilled,
			Resource:    podResource(o.Pod),
			Summary:     strings.Join(parts, "; "),
			Quote:       quote,
			EvidenceIDs: evidenceIDs(o.EvidenceID),
		})
	}
	return out
}

// oomTermination returns t when it looks like an OOM kill, otherwise nil.
func oomTermination(t *corev1.ContainerStateTerminated) *corev1.ContainerStateTerminated {
	if t != nil && (t.Reason == "OOMKilled" || t.ExitCode == oomExitCode) {
		return t
	}
	return nil
}

func memoryLimit(p *corev1.Pod, c container) string {
	if sp := c.spec(p); sp != nil {
		if q, ok := sp.Resources.Limits[corev1.ResourceMemory]; ok {
			return "memory limit " + q.String()
		}
	}
	return "no memory limit set"
}
