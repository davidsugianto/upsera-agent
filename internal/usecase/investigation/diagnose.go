package investigation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation/detect"
	"github.com/davidsugianto/upsera-agent/pkg/redact"
)

// Options selects the workload to diagnose.
type Options struct {
	Cluster   string
	Kind      string // deployment | statefulset | daemonset (short names allowed)
	Namespace string
	Name      string
	TailLines int64 // 0 means 100
	// MaxToolCalls caps Investigation.ToolCalls (INV-2); 0 means unlimited.
	MaxToolCalls int
}

// runDetectors is a seam so tests can assert on evidence wiring without
// depending on the detectors themselves.
var runDetectors = detect.Run

const defaultTailLines = 100

// Tool names Diagnose relies on; see the tool contract.
const (
	toolGet      = "k8s_get"
	toolListPods = "k8s_list_pods"
	toolDescribe = "k8s_describe"
	toolEvents   = "k8s_events"
	toolLogs     = "pod_logs"
	toolRollout  = "rollout_history"
)

// Diagnose collects evidence for one workload through reg, records every tool
// call in store, runs the detectors, and returns the investigation.
//
// Only an unreadable workload, a missing tool, a store failure, or a cancelled
// context abort the run. Any other failed call becomes an Unknown so a partial
// diagnosis is still returned. Failed calls produce no evidence row: there is
// no output to cite, so evidence IDs stay contiguous (E1..En) over stored rows.
func Diagnose(ctx context.Context, reg *tools.Registry, store EvidenceStore, opts Options) (*domain.Investigation, error) {
	kind := kube.NormalizeKind(opts.Kind)
	target := fmt.Sprintf("%s/%s/%s", kind, opts.Namespace, opts.Name)

	names := []string{toolGet, toolListPods, toolDescribe, toolEvents, toolLogs}
	if kind == "deployment" {
		names = append(names, toolRollout)
	}
	for _, n := range names {
		if _, ok := reg.Get(n); !ok {
			return nil, fmt.Errorf("diagnose %s: required tool %q is not registered", target, n)
		}
	}

	id, err := newInvestigationID()
	if err != nil {
		return nil, err
	}
	tail := opts.TailLines
	if tail <= 0 {
		tail = defaultTailLines
	}

	c := &collector{ctx: ctx, reg: reg, store: store, invID: id, max: opts.MaxToolCalls}
	start := time.Now().UTC()

	// Workload: nothing else can be done without it. A cap of 1 still lets it run.
	var snap detect.Snapshot
	res, evID, err := c.call(toolGet, map[string]any{"kind": kind, "namespace": opts.Namespace, "name": opts.Name})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", target, err)
	}
	wl, ok := res.Data.(*kube.Workload)
	if !ok || wl == nil {
		return nil, fmt.Errorf("read %s: unexpected %T from %s", target, res.Data, toolGet)
	}
	snap.Workload, snap.WorkloadEvidence = wl, evID

	// Calls kept back for the reads that follow the pods: the rollout history
	// and one node read per inspected node. Spending the whole cap on pods would
	// leave the node and rollout detectors with nothing to look at.
	tailReserve := 0
	if kind == "deployment" {
		tailReserve = 1
	}

	if len(wl.Selector) == 0 {
		c.unknown("%s has no pod selector; pods not inspected", target)
	} else {
		pods, err := c.pods(opts.Namespace, wl.Selector)
		if err != nil {
			return nil, err
		}
		if c.max > 0 {
			pods = podsByUrgency(pods)
		}
		nodes := map[string]bool{}
		for _, p := range pods {
			reserve := tailReserve + len(nodes)
			if n := p.Spec.NodeName; n != "" && !nodes[n] {
				reserve++
			}
			obs, inspected, err := c.observePod(opts.Namespace, p, tail, reserve)
			if err != nil {
				return nil, err
			}
			if !inspected {
				continue
			}
			if n := p.Spec.NodeName; n != "" {
				nodes[n] = true
			}
			snap.Pods = append(snap.Pods, obs)
		}
	}
	if c.fatal != nil {
		return nil, c.fatal
	}

	for _, node := range distinctNodes(snap.Pods) {
		if !c.affordable(tailReserve) {
			c.unknown("%s node/%s not read: tool call cap (%d) reached", toolGet, node, c.max)
			continue
		}
		res, evID, err := c.call(toolGet, map[string]any{"kind": "node", "name": node})
		if err != nil {
			if c.fatal != nil {
				return nil, c.fatal
			}
			c.unknown("%s node/%s: %v", toolGet, node, err)
			continue
		}
		n, ok := res.Data.(*corev1.Node)
		if !ok || n == nil {
			c.unknown("%s node/%s: unexpected %T", toolGet, node, res.Data)
			continue
		}
		snap.Nodes = append(snap.Nodes, detect.NodeObs{Node: n, EvidenceID: evID})
	}

	if kind == "deployment" {
		if !c.affordable(0) {
			c.unknown("%s %s not read: tool call cap (%d) reached", toolRollout, target, c.max)
		} else {
			res, evID, err := c.call(toolRollout, map[string]any{"namespace": opts.Namespace, "name": opts.Name})
			switch {
			case c.fatal != nil:
				return nil, c.fatal
			case err != nil:
				c.unknown("%s %s: %v", toolRollout, target, err)
			default:
				if revs, ok := res.Data.([]kube.Revision); ok {
					snap.Revisions, snap.RevisionsEvidence = revs, evID
				} else {
					c.unknown("%s %s: unexpected %T", toolRollout, target, res.Data)
				}
			}
		}
	}

	findings := sanitizeFindings(runDetectors(snap))
	unknowns := c.unknowns
	sort.Strings(unknowns)

	return &domain.Investigation{
		ID:         id,
		Cluster:    opts.Cluster,
		Trigger:    domain.Trigger{Type: domain.TriggerAsk, Name: target},
		Summary:    fmt.Sprintf("%d finding(s) for %s; %d unknown(s)", len(findings), target, len(unknowns)),
		Findings:   findings,
		Hypotheses: []domain.Hypothesis{},
		RuledOut:   []domain.RuledOut{},
		Unknowns:   unknowns,
		ToolCalls:  c.calls,
		StartedAt:  start,
		Duration:   time.Since(start),
	}, nil
}

// collector issues tool calls and records them. fatal is set when a call
// failed for a reason that must abort the run (store error, cancellation);
// call returns that error too, so callers can tell it from a tool failure by
// checking fatal.
type collector struct {
	ctx   context.Context
	reg   *tools.Registry
	store EvidenceStore
	invID string
	max   int // INV-2 cap on calls; 0 means unlimited

	calls    int
	nextID   int
	unknowns []string
	fatal    error
}

// errCapReached is returned by call instead of running a tool once the cap is
// spent. It is a backstop: callers check affordable first so that the cap is
// never exceeded and reserved calls are not eaten by earlier reads.
var errCapReached = errors.New("tool call cap reached")

// affordable reports whether one more call fits while keeping reserve calls
// back for later reads.
func (c *collector) affordable(reserve int) bool {
	return c.max == 0 || c.max-c.calls-reserve >= 1
}

// call runs one tool and stores its summary as evidence. The returned ID is
// empty when the call failed.
func (c *collector) call(name string, args map[string]any) (tools.Result, string, error) {
	if err := c.ctx.Err(); err != nil {
		c.fatal = err
		return tools.Result{}, "", err
	}
	t, ok := c.reg.Get(name)
	if !ok {
		c.fatal = fmt.Errorf("required tool %q is not registered", name)
		return tools.Result{}, "", c.fatal
	}
	raw, err := json.Marshal(args)
	if err != nil {
		c.fatal = fmt.Errorf("encode %s args: %w", name, err)
		return tools.Result{}, "", c.fatal
	}

	if !c.affordable(0) {
		return tools.Result{}, "", errCapReached
	}
	c.calls++
	res, err := t.Call(c.ctx, raw)
	if err != nil {
		if cerr := c.ctx.Err(); cerr != nil {
			c.fatal = cerr
		}
		return tools.Result{}, "", err
	}

	sum := sha256.Sum256([]byte(res.Summary))
	id := fmt.Sprintf("E%d", c.nextID+1)
	ev := domain.Evidence{
		ID:              id,
		InvestigationID: c.invID,
		Tool:            name,
		Args:            string(raw),
		Excerpt:         res.Summary,
		TS:              time.Now().UTC(),
		SHA256:          hex.EncodeToString(sum[:]),
	}
	if err := c.store.Add(c.ctx, ev); err != nil {
		c.fatal = fmt.Errorf("store evidence %s (%s): %w", id, name, err)
		return tools.Result{}, "", c.fatal
	}
	c.nextID++
	return res, id, nil
}

// unknown records a gap in the investigation. Messages are sanitized because
// tool errors can echo request details.
func (c *collector) unknown(format string, a ...any) {
	c.unknowns = append(c.unknowns, sanitize(fmt.Sprintf(format, a...)))
}

// sanitize makes cluster-derived text safe to print and store. Detectors quote
// kubelet messages and image references verbatim, and those are attacker- or
// workload-controlled, so they can carry credentials and terminal escape
// sequences. Newlines and tabs become spaces so a quote stays on one line;
// redact.Redact drops every other invisible character before it matches.
func sanitize(s string) string {
	return redact.Redact(strings.NewReplacer("\n", " ", "\t", " ").Replace(s)).Text
}

// sanitizeFindings returns a non-nil slice whose text fields went through sanitize.
func sanitizeFindings(fs []domain.Finding) []domain.Finding {
	out := make([]domain.Finding, len(fs))
	for i, f := range fs {
		f.Resource = sanitize(f.Resource)
		f.Summary = sanitize(f.Summary)
		f.Quote = sanitize(f.Quote)
		out[i] = f
	}
	return out
}

// pods lists the workload's pods. A failed listing is an Unknown, not fatal.
func (c *collector) pods(ns string, selector map[string]string) ([]corev1.Pod, error) {
	if !c.affordable(0) {
		c.unknown("%s not run: tool call cap (%d) reached", toolListPods, c.max)
		return nil, nil
	}
	res, _, err := c.call(toolListPods, map[string]any{"namespace": ns, "selector": selector})
	if c.fatal != nil {
		return nil, c.fatal
	}
	if err != nil {
		c.unknown("%s: %v", toolListPods, err)
		return nil, nil
	}
	pods, ok := res.Data.([]corev1.Pod)
	if !ok {
		c.unknown("%s: unexpected %T", toolListPods, res.Data)
		return nil, nil
	}
	return pods, nil
}

// observePod gathers describe, events and logs for one pod. The listed pod is
// kept as a fallback so node lookup and log selection still work when describe
// fails, but its EvidenceID stays empty so detectors will not cite it.
//
// reserve is how many calls must stay available for later reads. When the cap
// stops a call the pod is reported as an Unknown; inspected is false only when
// not even describe ran, so the caller leaves such a pod out of the snapshot.
func (c *collector) observePod(ns string, listed corev1.Pod, tail int64, reserve int) (obs detect.PodObs, inspected bool, err error) {
	obs = detect.PodObs{Pod: &listed}
	name := listed.Name

	// capped reports whether the next call must be skipped, recording why.
	capped := false
	skip := func() bool {
		if c.affordable(reserve) {
			return false
		}
		if !capped {
			capped = true
			if inspected {
				c.unknown("pod %s partially inspected: tool call cap (%d) reached", name, c.max)
			} else {
				c.unknown("pod %s not inspected: tool call cap (%d) reached", name, c.max)
			}
		}
		return true
	}

	if skip() {
		return obs, false, nil
	}
	inspected = true
	res, evID, err := c.call(toolDescribe, map[string]any{"namespace": ns, "pod": name})
	switch {
	case c.fatal != nil:
		return obs, inspected, c.fatal
	case err != nil:
		c.unknown("%s pod/%s: %v", toolDescribe, name, err)
	default:
		if p, ok := res.Data.(*corev1.Pod); ok && p != nil {
			obs.Pod, obs.EvidenceID = p, evID
		} else {
			c.unknown("%s pod/%s: unexpected %T", toolDescribe, name, res.Data)
		}
	}

	if skip() {
		return obs, inspected, nil
	}
	res, evID, err = c.call(toolEvents, map[string]any{"namespace": ns, "name": name})
	switch {
	case c.fatal != nil:
		return obs, inspected, c.fatal
	case err != nil:
		c.unknown("%s pod/%s: %v", toolEvents, name, err)
	default:
		if evs, ok := res.Data.([]corev1.Event); ok {
			obs.Events, obs.EventsEvidence = evs, evID
		} else {
			c.unknown("%s pod/%s: unexpected %T", toolEvents, name, res.Data)
		}
	}

	if skip() {
		return obs, inspected, nil
	}
	container, previous := logTarget(obs.Pod)
	args := map[string]any{"namespace": ns, "pod": name, "previous": previous, "tail_lines": tail}
	if container != "" {
		args["container"] = container
	}
	res, evID, err = c.call(toolLogs, args)
	switch {
	case c.fatal != nil:
		return obs, inspected, c.fatal
	case err != nil:
		c.unknown("%s pod/%s: %v", toolLogs, name, err)
	default:
		if logs, ok := res.Data.(string); ok {
			obs.Logs, obs.LogsEvidence = logs, evID
		} else {
			c.unknown("%s pod/%s: unexpected %T", toolLogs, name, res.Data)
		}
	}
	return obs, inspected, nil
}

// podsByUrgency returns a copy of pods ordered so the ones most likely to
// explain a failure are inspected first when the call cap cannot cover them
// all: not Ready, restarted, phase other than Running/Succeeded, a container
// waiting or terminated badly, then by name for a stable order.
func podsByUrgency(pods []corev1.Pod) []corev1.Pod {
	type ranked struct {
		pod  corev1.Pod
		keys [4]bool
	}
	rs := make([]ranked, len(pods))
	for i, p := range pods {
		rs[i] = ranked{pod: p, keys: [4]bool{!podReady(p), podRestarted(p), podPhaseOff(p), podContainerOff(p)}}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		for k := range rs[i].keys {
			if rs[i].keys[k] != rs[j].keys[k] {
				return rs[i].keys[k]
			}
		}
		return rs[i].pod.Name < rs[j].pod.Name
	})
	out := make([]corev1.Pod, len(rs))
	for i, r := range rs {
		out[i] = r.pod
	}
	return out
}

// podReady uses the Ready condition and falls back to container readiness for
// a pod that has not reported conditions yet.
func podReady(p corev1.Pod) bool {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	if len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func podRestarted(p corev1.Pod) bool {
	for _, cs := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.RestartCount > 0 {
			return true
		}
	}
	return false
}

func podPhaseOff(p corev1.Pod) bool {
	return p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodSucceeded
}

// podContainerOff is true for a container that is waiting or that exited
// non-zero. A clean exit is normal for a completed pod.
func podContainerOff(p corev1.Pod) bool {
	for _, cs := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.State.Waiting != nil {
			return true
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return true
		}
	}
	return false
}

// logTarget picks the container whose logs explain the failure. A restarted
// container's current instance is usually healthy-looking or empty, so its
// previous instance is read instead; otherwise the first terminated container
// is read as-is. Init containers are considered only when no regular container
// matches, and only a failed (non-zero exit) one counts, since a pod blocked in
// init has no regular container logs worth reading. An empty container lets
// the tool choose its default.
func logTarget(p *corev1.Pod) (container string, previous bool) {
	if p == nil {
		return "", false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > 0 {
			return cs.Name, true
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return cs.Name, false
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.RestartCount > 0 {
			return cs.Name, true
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return cs.Name, false
		}
	}
	return "", false
}

// distinctNodes returns node names in first-seen order, skipping pods that are
// not scheduled yet.
func distinctNodes(pods []detect.PodObs) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pods {
		if p.Pod == nil {
			continue
		}
		n := p.Pod.Spec.NodeName
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func newInvestigationID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate investigation id: %w", err)
	}
	return "inv-" + hex.EncodeToString(b[:]), nil
}
