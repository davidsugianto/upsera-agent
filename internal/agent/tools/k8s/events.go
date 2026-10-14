package k8stools

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

// maxEvents bounds the event lines in a Summary. Data still carries them all.
const maxEvents = 50

const timeFmt = time.RFC3339

const eventsSchema = `{
  "type": "object",
  "properties": {
    "namespace": {"type": "string"},
    "name": {"type": "string", "description": "name of the involved object, such as a pod or deployment"}
  },
  "required": ["namespace", "name"],
  "additionalProperties": false
}`

func newEvents(r *kube.Reader) tools.Tool {
	return &tool{
		name:   NameEvents,
		desc:   "List Kubernetes events for an object by name, oldest first (the newest are kept when the list is long).",
		schema: eventsSchema,
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
			evs, err := r.ListEvents(ctx, in.Namespace, in.Name)
			if err != nil {
				return tools.Result{}, fmt.Errorf("list events for %s/%s: %w", in.Namespace, in.Name, err)
			}
			shown, dropped := evs, 0
			if len(evs) > maxEvents {
				dropped = len(evs) - maxEvents
				shown = evs[dropped:]
			}
			var b strings.Builder
			if len(evs) == 0 {
				b.WriteString("no events\n")
			}
			if dropped > 0 {
				fmt.Fprintf(&b, "%s %d older events omitted\n", truncMarker, dropped)
			}
			for _, e := range shown {
				fmt.Fprintf(&b, "%s %s %s %s/%s: %s", eventStamp(e), e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
				if e.Count > 1 {
					fmt.Fprintf(&b, " (x%d)", e.Count)
				}
				b.WriteByte('\n')
			}
			res := finish(b.String(), evs, false)
			res.Truncated = res.Truncated || dropped > 0
			return res, nil
		},
	}
}

func eventStamp(e corev1.Event) string {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.UTC().Format(timeFmt)
	case !e.EventTime.IsZero():
		return e.EventTime.UTC().Format(timeFmt)
	}
	return e.CreationTimestamp.UTC().Format(timeFmt)
}
