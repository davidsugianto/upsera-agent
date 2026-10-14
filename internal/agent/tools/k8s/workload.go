package k8stools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

const getSchema = `{
  "type": "object",
  "properties": {
    "kind": {"type": "string", "description": "deployment, statefulset, daemonset or node (short names deploy, sts, ds accepted)"},
    "namespace": {"type": "string", "description": "required except for node"},
    "name": {"type": "string"}
  },
  "required": ["kind", "name"],
  "additionalProperties": false
}`

func newGet(r *kube.Reader) tools.Tool {
	return &tool{
		name:   NameGet,
		desc:   "Read a Deployment, StatefulSet, DaemonSet or Node: replica counts, rollout generation and status conditions.",
		schema: getSchema,
		call: func(ctx context.Context, args []byte) (tools.Result, error) {
			var in struct {
				Kind      string `json:"kind"`
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			}
			if err := decode(args, &in); err != nil {
				return tools.Result{}, err
			}
			if err := require("kind", in.Kind, "name", in.Name); err != nil {
				return tools.Result{}, err
			}
			kind := kube.NormalizeKind(in.Kind)
			if kind != "node" && !slices.Contains(kube.WorkloadKinds, kind) {
				return tools.Result{}, fmt.Errorf("%w: unsupported kind %q (use deployment, statefulset, daemonset, node)", ErrInvalidArgs, in.Kind)
			}
			if kind == "node" {
				n, err := r.GetNode(ctx, in.Name)
				if err != nil {
					return tools.Result{}, fmt.Errorf("get node %q: %w", in.Name, err)
				}
				return finish(renderNode(n), n, false), nil
			}
			if err := require("namespace", in.Namespace); err != nil {
				return tools.Result{}, err
			}
			w, err := r.GetWorkload(ctx, in.Kind, in.Namespace, in.Name)
			if err != nil {
				return tools.Result{}, fmt.Errorf("get %s %s/%s: %w", in.Kind, in.Namespace, in.Name, err)
			}
			return finish(renderWorkload(w), w, false), nil
		},
	}
}

func renderWorkload(w *kube.Workload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s/%s\n", w.Kind, w.Namespace, w.Name)
	fmt.Fprintf(&b, "replicas: desired=%d ready=%d updated=%d\n", w.Desired, w.Ready, w.Updated)
	fmt.Fprintf(&b, "generation: %d observed=%d\n", w.Generation, w.ObservedGeneration)
	fmt.Fprintf(&b, "selector: %s\n", fmtMap(w.Selector))
	for _, c := range w.Conditions {
		fmt.Fprintf(&b, "condition: %s=%s reason=%s message=%q\n", c.Type, c.Status, c.Reason, c.Message)
	}
	return b.String()
}

func renderNode(n *corev1.Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Node %s\n", n.Name)
	fmt.Fprintf(&b, "unschedulable: %t\n", n.Spec.Unschedulable)
	for _, c := range n.Status.Conditions {
		fmt.Fprintf(&b, "condition: %s=%s reason=%s message=%q\n", c.Type, c.Status, c.Reason, c.Message)
	}
	for _, t := range n.Spec.Taints {
		fmt.Fprintf(&b, "taint: %s=%s:%s\n", t.Key, t.Value, t.Effect)
	}
	if a := n.Status.Allocatable; len(a) > 0 {
		fmt.Fprintf(&b, "allocatable: cpu=%s memory=%s pods=%s\n",
			a.Cpu().String(), a.Memory().String(), a.Pods().String())
	}
	return b.String()
}

func fmtMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

const rolloutSchema = `{
  "type": "object",
  "properties": {
    "namespace": {"type": "string"},
    "name": {"type": "string", "description": "Deployment name"}
  },
  "required": ["namespace", "name"],
  "additionalProperties": false
}`

func newRolloutHistory(r *kube.Reader) tools.Tool {
	return &tool{
		name:   NameRolloutHistory,
		desc:   "List the ReplicaSet revisions of a Deployment, newest first, with images and ready counts.",
		schema: rolloutSchema,
		call: func(ctx context.Context, args []byte) (tools.Result, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			}
			if err := decode(args, &in); err != nil {
				return tools.Result{}, err
			}
			if err := require("namespace", in.Namespace, "name", in.Name); err != nil {
				return tools.Result{}, err
			}
			revs, err := r.RolloutHistory(ctx, in.Namespace, in.Name)
			if err != nil {
				return tools.Result{}, fmt.Errorf("rollout history %s/%s: %w", in.Namespace, in.Name, err)
			}
			var b strings.Builder
			if len(revs) == 0 {
				b.WriteString("no ReplicaSets owned by the deployment\n")
			}
			for _, v := range revs {
				fmt.Fprintf(&b, "revision %d %s replicas=%d ready=%d created=%s images=%s\n",
					v.Revision, v.Name, v.Replicas, v.Ready, v.Created.UTC().Format(timeFmt), strings.Join(v.Images, ","))
			}
			return finish(b.String(), revs, false), nil
		},
	}
}
