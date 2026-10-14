package k8stools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

const (
	defaultTailLines = 100
	maxTailLines     = 200
)

const listPodsSchema = `{
  "type": "object",
  "properties": {
    "namespace": {"type": "string"},
    "selector": {"type": "object", "additionalProperties": {"type": "string"}, "minProperties": 1,
                 "description": "label selector, normally the workload's selector"}
  },
  "required": ["namespace", "selector"],
  "additionalProperties": false
}`

func newListPods(r *kube.Reader) tools.Tool {
	return &tool{
		name:   NameListPods,
		desc:   "List pods matching a label selector: phase, ready containers, restarts and node. An empty selector is refused.",
		schema: listPodsSchema,
		call: func(ctx context.Context, args []byte) (tools.Result, error) {
			var in struct {
				Namespace string            `json:"namespace"`
				Selector  map[string]string `json:"selector"`
			}
			if err := decode(args, &in); err != nil {
				return tools.Result{}, err
			}
			if err := require("namespace", in.Namespace); err != nil {
				return tools.Result{}, err
			}
			if len(in.Selector) == 0 {
				return tools.Result{}, fmt.Errorf("%w: \"selector\" must not be empty", ErrInvalidArgs)
			}
			pods, err := r.ListPods(ctx, in.Namespace, in.Selector)
			if err != nil {
				return tools.Result{}, fmt.Errorf("list pods in %s: %w", in.Namespace, err)
			}
			var b strings.Builder
			if len(pods) == 0 {
				b.WriteString("no pods match the selector\n")
			}
			for i := range pods {
				p := &pods[i]
				ready, restarts := 0, int32(0)
				for _, cs := range p.Status.ContainerStatuses {
					if cs.Ready {
						ready++
					}
					restarts += cs.RestartCount
				}
				fmt.Fprintf(&b, "%s phase=%s ready=%d/%d restarts=%d node=%s\n",
					p.Name, p.Status.Phase, ready, len(p.Spec.Containers), restarts, p.Spec.NodeName)
			}
			return finish(b.String(), pods, false), nil
		},
	}
}

const describeSchema = `{
  "type": "object",
  "properties": {
    "namespace": {"type": "string"},
    "pod": {"type": "string"}
  },
  "required": ["namespace", "pod"],
  "additionalProperties": false
}`

func newDescribe(r *kube.Reader) tools.Tool {
	return &tool{
		name:   NameDescribe,
		desc:   "Describe a pod: phase, conditions, per-container state, last termination, resources and probes. Environment variable names are listed, never values.",
		schema: describeSchema,
		call: func(ctx context.Context, args []byte) (tools.Result, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Pod       string `json:"pod"`
			}
			if err := decode(args, &in); err != nil {
				return tools.Result{}, err
			}
			if err := require("namespace", in.Namespace, "pod", in.Pod); err != nil {
				return tools.Result{}, err
			}
			p, err := r.GetPod(ctx, in.Namespace, in.Pod)
			if err != nil {
				return tools.Result{}, fmt.Errorf("get pod %s/%s: %w", in.Namespace, in.Pod, err)
			}
			return finish(renderPod(p), p, false), nil
		},
	}
}

// renderPod writes the diagnosis-relevant view of a pod. It never touches
// EnvVar.Value, ConfigMaps, or Secrets: env entries are names only, because
// pattern redaction cannot be trusted to catch an arbitrary secret value.
func renderPod(p *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\nnamespace: %s\nnode: %s\nphase: %s\n", p.Name, p.Namespace, p.Spec.NodeName, p.Status.Phase)
	if p.Status.Reason != "" || p.Status.Message != "" {
		fmt.Fprintf(&b, "reason: %s message=%q\n", p.Status.Reason, p.Status.Message)
	}
	for _, c := range p.Status.Conditions {
		fmt.Fprintf(&b, "condition: %s=%s", c.Type, c.Status)
		if c.Reason != "" {
			fmt.Fprintf(&b, " reason=%s", c.Reason)
		}
		if c.Message != "" {
			fmt.Fprintf(&b, " message=%q", c.Message)
		}
		b.WriteByte('\n')
	}
	renderContainers(&b, "initContainer", p.Spec.InitContainers, p.Status.InitContainerStatuses)
	renderContainers(&b, "container", p.Spec.Containers, p.Status.ContainerStatuses)
	return b.String()
}

func renderContainers(b *strings.Builder, label string, specs []corev1.Container, statuses []corev1.ContainerStatus) {
	byName := make(map[string]corev1.ContainerStatus, len(statuses))
	for _, s := range statuses {
		byName[s.Name] = s
	}
	for _, c := range specs {
		fmt.Fprintf(b, "%s: %s image=%s", label, c.Name, c.Image)
		if s, ok := byName[c.Name]; ok {
			fmt.Fprintf(b, " ready=%t restartCount=%d\n", s.Ready, s.RestartCount)
			fmt.Fprintf(b, "  state: %s\n", fmtState(s.State))
			if last := fmtState(s.LastTerminationState); last != "none" {
				fmt.Fprintf(b, "  lastState: %s\n", last)
			}
		} else {
			b.WriteString(" (no status yet)\n")
		}
		if res := fmtResources(c.Resources); res != "" {
			fmt.Fprintf(b, "  resources: %s\n", res)
		}
		for _, pr := range []struct {
			kind string
			p    *corev1.Probe
		}{{"liveness", c.LivenessProbe}, {"readiness", c.ReadinessProbe}, {"startup", c.StartupProbe}} {
			if pr.p != nil {
				fmt.Fprintf(b, "  probe: %s %s\n", pr.kind, fmtProbe(pr.p))
			}
		}
		if names := envNames(c); len(names) > 0 {
			fmt.Fprintf(b, "  env: %s\n", strings.Join(names, ", "))
		}
	}
}

func fmtState(s corev1.ContainerState) string {
	switch {
	case s.Waiting != nil:
		return fmt.Sprintf("waiting reason=%s message=%q", s.Waiting.Reason, s.Waiting.Message)
	case s.Running != nil:
		return "running since " + s.Running.StartedAt.UTC().Format(timeFmt)
	case s.Terminated != nil:
		t := s.Terminated
		return fmt.Sprintf("terminated reason=%s exitCode=%d finishedAt=%s", t.Reason, t.ExitCode, t.FinishedAt.UTC().Format(timeFmt))
	}
	return "none"
}

func fmtResources(r corev1.ResourceRequirements) string {
	var parts []string
	for _, g := range []struct {
		label string
		list  corev1.ResourceList
	}{{"requests", r.Requests}, {"limits", r.Limits}} {
		if len(g.list) == 0 {
			continue
		}
		names := make([]string, 0, len(g.list))
		for n := range g.list {
			names = append(names, string(n))
		}
		sort.Strings(names)
		kv := make([]string, len(names))
		for i, n := range names {
			q := g.list[corev1.ResourceName(n)]
			kv[i] = n + "=" + q.String()
		}
		parts = append(parts, g.label+" "+strings.Join(kv, " "))
	}
	return strings.Join(parts, "; ")
}

// fmtProbe names the handler and timing only. Exec commands are not printed:
// they can embed credentials.
func fmtProbe(p *corev1.Probe) string {
	var h string
	switch {
	case p.HTTPGet != nil:
		h = fmt.Sprintf("http GET %s:%s", p.HTTPGet.Path, p.HTTPGet.Port.String())
	case p.TCPSocket != nil:
		h = "tcp " + p.TCPSocket.Port.String()
	case p.GRPC != nil:
		h = fmt.Sprintf("grpc :%d", p.GRPC.Port)
	case p.Exec != nil:
		h = "exec"
	default:
		h = "unknown"
	}
	return fmt.Sprintf("%s delay=%ds period=%ds timeout=%ds failureThreshold=%d",
		h, p.InitialDelaySeconds, p.PeriodSeconds, p.TimeoutSeconds, p.FailureThreshold)
}

// envNames lists variable names and the names of the ConfigMaps and Secrets
// bulk-imported with envFrom, which helps explain config-related start
// failures without exposing any value.
func envNames(c corev1.Container) []string {
	var out []string
	for _, e := range c.Env {
		out = append(out, e.Name)
	}
	for _, f := range c.EnvFrom {
		switch {
		case f.ConfigMapRef != nil:
			out = append(out, "envFrom:configmap/"+f.ConfigMapRef.Name)
		case f.SecretRef != nil:
			out = append(out, "envFrom:secret/"+f.SecretRef.Name)
		}
	}
	return out
}

const podLogsSchema = `{
  "type": "object",
  "properties": {
    "namespace": {"type": "string"},
    "pod": {"type": "string"},
    "container": {"type": "string", "description": "optional when the pod has one container"},
    "previous": {"type": "boolean", "description": "read the previous (crashed) container instance"},
    "tail_lines": {"type": "integer", "minimum": 1, "maximum": 200, "default": 100}
  },
  "required": ["namespace", "pod"],
  "additionalProperties": false
}`

func newPodLogs(r *kube.Reader) tools.Tool {
	return &tool{
		name:   NamePodLogs,
		desc:   "Read the redacted tail of a container log (default 100 lines, at most 200).",
		schema: podLogsSchema,
		call: func(ctx context.Context, args []byte) (tools.Result, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Pod       string `json:"pod"`
				Container string `json:"container"`
				Previous  bool   `json:"previous"`
				TailLines *int   `json:"tail_lines"`
			}
			if err := decode(args, &in); err != nil {
				return tools.Result{}, err
			}
			if err := require("namespace", in.Namespace, "pod", in.Pod); err != nil {
				return tools.Result{}, err
			}
			tail := int64(defaultTailLines)
			if in.TailLines != nil {
				if *in.TailLines < 1 {
					return tools.Result{}, fmt.Errorf("%w: \"tail_lines\" must be at least 1", ErrInvalidArgs)
				}
				tail = min(int64(*in.TailLines), maxTailLines)
			}
			raw, err := r.PodLogs(ctx, in.Namespace, in.Pod, in.Container, in.Previous, tail)
			if err != nil {
				return tools.Result{}, fmt.Errorf("logs for %s/%s: %w", in.Namespace, in.Pod, err)
			}
			// Data is the whole redacted tail; Summary is its capped form.
			res, full := finishRedacted(raw, true)
			res.Data = full
			return res, nil
		},
	}
}
