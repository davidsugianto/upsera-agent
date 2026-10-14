// Package llm is the only place that knows which model vendor is in use
// (ADR 0003). The rest of the agent sees Client: messages in, JSON that
// conforms to a schema out, plus token usage. Tool calling joins the interface
// with the investigation loop in Phase 2; Phase 1 needs structured output only.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// Message is one conversation turn. Role is "user" or "assistant".
type Message struct {
	Role    string
	Content string
}

// Request asks for one completion whose text is a JSON document that conforms
// to Schema. Adapters must enforce the schema with the provider's structured
// output feature, never by parsing free text. Callers must not set sampling
// parameters: several current models reject non-default values.
type Request struct {
	Model      string
	System     string
	Messages   []Message
	SchemaName string          // short identifier, [a-zA-Z0-9_-]
	Schema     json.RawMessage // JSON Schema for the reply
	MaxTokens  int             // hard cap on output tokens
}

// Usage is what one call cost in tokens, as reported by the provider.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Total is input plus output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Response is one completion. A refusal is a normal outcome, not an error: the
// provider declined, Text is empty, and the caller decides how to degrade.
type Response struct {
	Text            string
	Usage           Usage
	Model           string
	StopReason      string
	Refused         bool
	RefusalCategory string
}

// Client sends one request to one model.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

var (
	// ErrBudgetExceeded means a call or token budget stopped the request.
	ErrBudgetExceeded = errors.New("llm budget exceeded")
	// ErrEmptyOutput means the provider returned no text and did not refuse.
	ErrEmptyOutput = errors.New("llm returned no output")
)
