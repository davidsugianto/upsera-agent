package detect

import (
	"sort"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// Detector names. They appear in findings and in the contract with the
// evaluation harness, so they must not change.
const (
	detCrashloop     = "crashloop"
	detOOMKilled     = "oomkilled"
	detImagePull     = "image_pull"
	detUnschedulable = "unschedulable"
	detProbeFailure  = "probe_failure"
	detNodeNotReady  = "node_not_ready"
	detEvicted       = "evicted"
	detRolloutStuck  = "rollout_stuck"
)

// detectors is the fixed execution order; findings come back in this order.
var detectors = []func(Snapshot) []domain.Finding{
	crashloop,
	oomkilled,
	imagePull,
	unschedulable,
	probeFailure,
	nodeNotReady,
	evicted,
	rolloutStuck,
}

// Run applies every detector to the snapshot. Output order is detector order,
// then resource name, so repeated runs over the same snapshot are identical.
func Run(s Snapshot) []domain.Finding {
	var out []domain.Finding
	for _, d := range detectors {
		fs := d(s)
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Resource < fs[j].Resource })
		out = append(out, fs...)
	}
	return out
}
