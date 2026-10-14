package investigation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	c := &collector{ctx: ctx, reg: reg, store: store, invID: id}
	start := time.Now().UTC()

	// Workload: nothing else can be done without it.
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

	if len(wl.Selector) == 0 {
		c.unknown("%s has no pod selector; pods not inspected", target)
	} else {
		pods, err := c.pods(opts.Namespace, wl.Selector)
		if err != nil {
			return nil, err
		}
		for _, p := range pods {
			obs, err := c.observePod(opts.Namespace, p, tail)
			if err != nil {
				return nil, err
			}
			snap.Pods = append(snap.Pods, obs)
		}
	}
	if c.fatal != nil {
		return nil, c.fatal
	}

	for _, node := range distinctNodes(snap.Pods) {
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

	calls    int
	nextID   int
	unknowns []string
	fatal    error
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
func (c *collector) observePod(ns string, listed corev1.Pod, tail int64) (detect.PodObs, error) {
	obs := detect.PodObs{Pod: &listed}
	name := listed.Name

	res, evID, err := c.call(toolDescribe, map[string]any{"namespace": ns, "pod": name})
	switch {
	case c.fatal != nil:
		return obs, c.fatal
	case err != nil:
		c.unknown("%s pod/%s: %v", toolDescribe, name, err)
	default:
		if p, ok := res.Data.(*corev1.Pod); ok && p != nil {
			obs.Pod, obs.EvidenceID = p, evID
		} else {
			c.unknown("%s pod/%s: unexpected %T", toolDescribe, name, res.Data)
		}
	}

	res, evID, err = c.call(toolEvents, map[string]any{"namespace": ns, "name": name})
	switch {
	case c.fatal != nil:
		return obs, c.fatal
	case err != nil:
		c.unknown("%s pod/%s: %v", toolEvents, name, err)
	default:
		if evs, ok := res.Data.([]corev1.Event); ok {
			obs.Events, obs.EventsEvidence = evs, evID
		} else {
			c.unknown("%s pod/%s: unexpected %T", toolEvents, name, res.Data)
		}
	}

	container, previous := logTarget(obs.Pod)
	args := map[string]any{"namespace": ns, "pod": name, "previous": previous, "tail_lines": tail}
	if container != "" {
		args["container"] = container
	}
	res, evID, err = c.call(toolLogs, args)
	switch {
	case c.fatal != nil:
		return obs, c.fatal
	case err != nil:
		c.unknown("%s pod/%s: %v", toolLogs, name, err)
	default:
		if logs, ok := res.Data.(string); ok {
			obs.Logs, obs.LogsEvidence = logs, evID
		} else {
			c.unknown("%s pod/%s: unexpected %T", toolLogs, name, res.Data)
		}
	}
	return obs, nil
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
