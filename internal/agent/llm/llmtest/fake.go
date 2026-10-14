// Package llmtest holds a scriptable llm.Client for tests.
package llmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
)

// Fake replays Responses in order and records every Request. Set the exported
// fields before use and read Requests only after the calls under test finish.
type Fake struct {
	Responses []llm.Response
	Errs      []error // optional, index-aligned with Responses
	Requests  []llm.Request

	mu sync.Mutex
}

// Complete returns the next scripted outcome. When Errs[i] is non-nil it is
// returned alone; otherwise Responses[i] is. Running past the script is an
// error so a test never silently passes on missing model calls.
func (f *Fake) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	i := len(f.Requests)
	f.Requests = append(f.Requests, copyRequest(req))

	if i >= len(f.Responses) {
		return llm.Response{}, fmt.Errorf("llmtest.Fake: no scripted response for call %d (only %d scripted)", i+1, len(f.Responses))
	}
	if i < len(f.Errs) && f.Errs[i] != nil {
		return llm.Response{}, f.Errs[i]
	}
	return f.Responses[i], nil
}

// JSON returns a response whose text is v marshaled, with small fixed usage.
// It panics if v cannot be marshaled: that is a bug in the test.
func JSON(v any) llm.Response {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("llmtest.JSON: %v", err))
	}
	return llm.Response{
		Text:       string(b),
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
		StopReason: "end_turn",
	}
}

// copyRequest detaches slices so later mutation by the caller cannot change
// what a test asserts on.
func copyRequest(r llm.Request) llm.Request {
	r.Messages = slices.Clone(r.Messages)
	r.Schema = slices.Clone(r.Schema)
	return r
}
