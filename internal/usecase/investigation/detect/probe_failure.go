package detect

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// probeFailure reports pods whose probes are failing, from two kinds of
// evidence only: Unhealthy events naming a failed readiness or liveness probe,
// or a running container that is not Ready although it has a readiness probe
// and nothing else about the pod explains it (no waiting or failed container).
func probeFailure(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, o := range s.Pods {
		if o.Pod == nil {
			continue
		}
		var ids []string
		var parts []string
		var quote string

		if o.EventsEvidence != "" {
			if msg := probeEvent(o.Events, allReady(o.Pod)); msg != "" {
				parts = append(parts, "events report a failing probe")
				quote = msg
				ids = append(ids, o.EventsEvidence)
			}
		}
		if o.EvidenceID != "" {
			if names := unreadyRunning(o.Pod); len(names) > 0 {
				parts = append(parts, fmt.Sprintf("%s is running but not Ready (readiness probe configured)", strings.Join(names, ", ")))
				if quote == "" {
					quote = "Ready=false"
				}
				ids = append(ids, o.EvidenceID)
			}
		}
		if len(parts) == 0 {
			continue
		}
		if o.EvidenceID != "" {
			ids = append([]string{o.EvidenceID}, ids...)
		}
		out = append(out, domain.Finding{
			Detector:    detProbeFailure,
			Resource:    podResource(o.Pod),
			Summary:     "probe failing: " + strings.Join(parts, "; "),
			Quote:       quote,
			EvidenceIDs: evidenceIDs(ids...),
		})
	}
	return out
}

// probeEvent returns the message of the first Unhealthy event that reports a
// failed readiness or liveness probe. Events outlive the problem: every pod
// logs a readiness failure while it starts, so a readiness event only counts
// while the pod is still not Ready. A liveness failure restarts the container,
// which leaves the pod Ready again, so it always counts.
func probeEvent(evs []corev1.Event, ready bool) string {
	for _, e := range evs {
		if e.Reason != "Unhealthy" {
			continue
		}
		m := strings.ToLower(e.Message)
		if !strings.Contains(m, "probe failed") {
			continue
		}
		if strings.Contains(m, "liveness") || (strings.Contains(m, "readiness") && !ready) {
			return e.Message
		}
	}
	return ""
}

// allReady reports whether every regular container is Ready.
func allReady(p *corev1.Pod) bool {
	if p == nil || len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// unreadyRunning returns the labels of running, not-Ready containers that have
// a readiness probe, or nil when another cause is visible on the pod.
func unreadyRunning(p *corev1.Pod) []string {
	if p.Status.Phase != corev1.PodRunning {
		return nil
	}
	var names []string
	for _, c := range containers(p) {
		switch {
		case c.State.Waiting != nil:
			return nil
		case c.State.Terminated != nil:
			if !c.init || c.State.Terminated.ExitCode != 0 {
				return nil
			}
		case !c.init && c.State.Running != nil && !c.Ready:
			if sp := c.spec(p); sp != nil && sp.ReadinessProbe != nil {
				names = append(names, c.label())
			}
		}
	}
	return names
}
