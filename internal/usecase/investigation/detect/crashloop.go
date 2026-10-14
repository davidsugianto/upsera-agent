package detect

import (
	"fmt"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// crashloopRestarts is how many restarts, together with a terminated state,
// count as a crash loop even before the kubelet reports CrashLoopBackOff.
const crashloopRestarts = 3

// crashloop reports containers (init included) that are in CrashLoopBackOff or
// have restarted repeatedly and are currently terminated. An init container
// that retried and then exited 0 finished its job and is not reported.
func crashloop(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, o := range s.Pods {
		if !o.usable() {
			continue
		}
		var parts []string
		var quote string
		for _, c := range containers(o.Pod) {
			var part, q string
			if w := c.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
				part = fmt.Sprintf("%s is in CrashLoopBackOff after %d restarts", c.label(), c.RestartCount)
				q = reasonMessage(w.Reason, w.Message)
			} else if t := c.State.Terminated; t != nil && c.RestartCount >= crashloopRestarts && (!c.init || t.ExitCode != 0) {
				q = firstNonEmpty(t.Reason, fmt.Sprintf("exit code %d", t.ExitCode))
				part = fmt.Sprintf("%s restarted %d times and is terminated (%s, exit code %d)", c.label(), c.RestartCount, q, t.ExitCode)
			} else {
				continue
			}
			parts = append(parts, part)
			if quote == "" {
				quote = q
			}
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, domain.Finding{
			Detector:    detCrashloop,
			Resource:    podResource(o.Pod),
			Summary:     strings.Join(parts, "; "),
			Quote:       quote,
			EvidenceIDs: evidenceIDs(o.EvidenceID),
		})
	}
	return out
}
