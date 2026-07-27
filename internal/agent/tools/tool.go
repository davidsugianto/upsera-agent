// Package tools defines the only channel through which the agent may observe
// or affect the world. The agent never executes arbitrary shell or kubectl
// commands: every capability is a typed, schema-validated tool, which keeps
// the blast radius auditable and the behavior testable.
package tools

import "context"

// Result is what returns to the model context. Summary is compacted and
// redacted; the full output is written to storage and referenced by RawRef so
// that a large cluster cannot exhaust the context window.
type Result struct {
	Summary   string
	RawRef    string
	Truncated bool
}

// Tool is the interface every capability implements. Schema is the JSON Schema
// advertised to the model. Mutating reports whether the tool can change cluster
// state; the registry refuses to load a mutating tool unless remediation is
// explicitly enabled.
type Tool interface {
	Name() string
	Description() string
	Schema() []byte
	Mutating() bool
	Call(ctx context.Context, args []byte) (Result, error)
}

// Registry holds the tools available to a given investigation. Scoping is
// per-investigation rather than global so that namespace allowlists and the
// Phase 2 remediation gate can be enforced at construction time rather than
// checked at call time.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry builds a registry. allowMutating must come from an explicit
// operator decision, never from a model-produced value.
func NewRegistry(allowMutating bool, ts ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		if t.Mutating() && !allowMutating {
			continue
		}
		r.tools[t.Name()] = t
	}
	return r
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names lists the registered tool names.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	return out
}
