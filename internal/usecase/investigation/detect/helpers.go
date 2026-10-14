package detect

import (
	corev1 "k8s.io/api/core/v1"
)

// container pairs a container status with whether it is an init container.
type container struct {
	corev1.ContainerStatus
	init bool
}

// label names the container the way summaries refer to it.
func (c container) label() string {
	if c.init {
		return `init container "` + c.Name + `"`
	}
	return `container "` + c.Name + `"`
}

// containers lists init container statuses first, then regular ones.
func containers(p *corev1.Pod) []container {
	var out []container
	for _, cs := range p.Status.InitContainerStatuses {
		out = append(out, container{cs, true})
	}
	for _, cs := range p.Status.ContainerStatuses {
		out = append(out, container{cs, false})
	}
	return out
}

// spec returns the pod spec entry for a container, if there is one.
func (c container) spec(p *corev1.Pod) *corev1.Container {
	list := p.Spec.Containers
	if c.init {
		list = p.Spec.InitContainers
	}
	for i := range list {
		if list[i].Name == c.Name {
			return &list[i]
		}
	}
	return nil
}

// usable reports whether a pod observation may back a finding: the pod must be
// present and its describe call must have produced stored evidence.
func (o PodObs) usable() bool { return o.Pod != nil && o.EvidenceID != "" }

func podResource(p *corev1.Pod) string { return "pod/" + p.Name }

// evidenceIDs drops empty IDs and duplicates, keeping first-seen order.
func evidenceIDs(ids ...string) []string {
	seen := make(map[string]bool, len(ids))
	var out []string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// reasonMessage renders "reason: message", or whichever of the two exists.
func reasonMessage(reason, msg string) string {
	switch {
	case reason != "" && msg != "":
		return reason + ": " + msg
	case reason != "":
		return reason
	default:
		return msg
	}
}
