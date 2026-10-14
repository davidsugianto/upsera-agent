package detect

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// nodeNotReady reports nodes that are not Ready or are under memory, disk or
// PID pressure, but only nodes a pod of the workload runs on. The finding also
// cites those pods' describe evidence, which is what shows the placement.
func nodeNotReady(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, n := range s.Nodes {
		if n.Node == nil || n.EvidenceID == "" {
			continue
		}
		problems, quote := nodeProblems(n.Node)
		if len(problems) == 0 {
			continue
		}
		var pods, ids []string
		for _, o := range s.Pods {
			if o.Pod != nil && o.Pod.Spec.NodeName == n.Node.Name {
				pods = append(pods, o.Pod.Name)
				ids = append(ids, o.EvidenceID)
			}
		}
		if len(pods) == 0 {
			continue
		}
		sort.Strings(pods)
		out = append(out, domain.Finding{
			Detector:    detNodeNotReady,
			Resource:    "node/" + n.Node.Name,
			Summary:     fmt.Sprintf("node %s: %s; pods on it: %s", n.Node.Name, strings.Join(problems, "; "), strings.Join(pods, ", ")),
			Quote:       quote,
			EvidenceIDs: evidenceIDs(append(ids, n.EvidenceID)...),
		})
	}
	return out
}

// pressureTypes are the node conditions that are bad when True.
var pressureTypes = []corev1.NodeConditionType{corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure}

// nodeProblems describes each failing condition and quotes the first one.
func nodeProblems(n *corev1.Node) (problems []string, quote string) {
	byType := make(map[corev1.NodeConditionType]corev1.NodeCondition, len(n.Status.Conditions))
	for _, c := range n.Status.Conditions {
		byType[c.Type] = c
	}
	add := func(p string, c corev1.NodeCondition) {
		problems = append(problems, p)
		if quote == "" {
			quote = firstNonEmpty(c.Message, c.Reason)
		}
	}
	if c, ok := byType[corev1.NodeReady]; ok && c.Status != corev1.ConditionTrue {
		add(fmt.Sprintf("not Ready (Ready=%s, %s)", c.Status, firstNonEmpty(c.Reason, "no reason")), c)
	}
	for _, t := range pressureTypes {
		if c, ok := byType[t]; ok && c.Status == corev1.ConditionTrue {
			add(fmt.Sprintf("%s (%s)", t, firstNonEmpty(c.Reason, "no reason")), c)
		}
	}
	return problems, quote
}
