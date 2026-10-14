// Package k8stools holds the typed read-only Kubernetes tools (ADR 0001).
// Each tool wraps one kube.Reader method, so the namespace filter and the
// get/list/log-only access are enforced in one place.
package k8stools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	"github.com/davidsugianto/upsera-agent/pkg/redact"

	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
)

// Tool names, as in the PRD.
const (
	NameGet            = "k8s_get"
	NameListPods       = "k8s_list_pods"
	NameDescribe       = "k8s_describe"
	NameEvents         = "k8s_events"
	NamePodLogs        = "pod_logs"
	NameRolloutHistory = "rollout_history"
)

// maxSummaryBytes caps a Summary. Summaries are what gets stored as evidence
// and shown to a model, so they stay small regardless of cluster size.
const maxSummaryBytes = 8 << 10

const truncMarker = "[truncated]"

// ErrInvalidArgs is wrapped by every argument validation failure, so callers
// can tell a malformed call from a cluster error.
var ErrInvalidArgs = errors.New("invalid arguments")

// NewTools returns the six read-only tools backed by r.
func NewTools(r *kube.Reader) []tools.Tool {
	return []tools.Tool{
		newGet(r),
		newListPods(r),
		newDescribe(r),
		newEvents(r),
		newPodLogs(r),
		newRolloutHistory(r),
	}
}

// tool is the shared shell around one call function. None of the tools can
// mutate the cluster, so Mutating is fixed rather than a field.
type tool struct {
	name, desc, schema string
	call               func(ctx context.Context, args []byte) (tools.Result, error)
}

func (t *tool) Name() string        { return t.name }
func (t *tool) Description() string { return t.desc }
func (t *tool) Schema() []byte      { return []byte(t.schema) }
func (t *tool) Mutating() bool      { return false }

func (t *tool) Call(ctx context.Context, args []byte) (tools.Result, error) {
	res, err := t.call(ctx, args)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", t.name, err)
	}
	return res, nil
}

// decode parses args strictly: unknown fields, malformed JSON, and trailing
// data are all rejected so a model cannot smuggle intent past the schema.
func decode(args []byte, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return fmt.Errorf("%w: arguments are empty", ErrInvalidArgs)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArgs, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON object", ErrInvalidArgs)
	}
	return nil
}

// require rejects blank values for the named required fields.
func require(fields ...string) error {
	for i := 0; i+1 < len(fields); i += 2 {
		if strings.TrimSpace(fields[i+1]) == "" {
			return fmt.Errorf("%w: %q is required", ErrInvalidArgs, fields[i])
		}
	}
	return nil
}

// finish redacts text, then truncates it. Redaction runs first so a secret
// cut in half by the cap cannot slip past a pattern. keepTail truncates from
// the front, which is what logs want: the newest lines matter most.
func finish(text string, data any, keepTail bool) tools.Result {
	res, _ := finishRedacted(text, keepTail)
	res.Data = data
	return res
}

// finishRedacted is finish for tools whose Data is the redacted text itself;
// it also returns that full redacted text.
func finishRedacted(text string, keepTail bool) (tools.Result, string) {
	red := redact.Redact(text)
	out, cut := capText(red.Text, keepTail)
	return tools.Result{Summary: out, Truncated: cut, Redactions: red.Counts}, red.Text
}

func capText(s string, keepTail bool) (string, bool) {
	if len(s) <= maxSummaryBytes {
		return s, false
	}
	if keepTail {
		i := len(s) - maxSummaryBytes
		for i < len(s) && !utf8.RuneStart(s[i]) {
			i++
		}
		return truncMarker + "\n" + s[i:], true
	}
	i := maxSummaryBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "\n" + truncMarker, true
}
