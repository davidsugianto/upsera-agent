package llm

import (
	"context"
	"fmt"
	"sync"
)

// Budget bounds one investigation's model use (INV-2). Zero means unlimited.
type Budget struct {
	MaxCalls  int
	MaxTokens int // input plus output, summed over calls
}

// Budgeted wraps a Client and enforces a Budget. It is safe for concurrent use.
type Budgeted struct {
	inner  Client
	budget Budget

	mu       sync.Mutex
	calls    int
	used     Usage
	reserved int // worst-case tokens of calls still in flight
}

// NewBudgeted wraps c.
func NewBudgeted(c Client, b Budget) *Budgeted { return &Budgeted{inner: c, budget: b} }

// Complete refuses the request before it leaves the process when it could not
// fit in the remaining budget, so an over-budget call never costs money.
func (b *Budgeted) Complete(ctx context.Context, req Request) (Response, error) {
	worst := estimateInputTokens(req) + req.MaxTokens

	b.mu.Lock()
	if b.budget.MaxCalls > 0 && b.calls >= b.budget.MaxCalls {
		b.mu.Unlock()
		return Response{}, fmt.Errorf("%w: %d calls already made (max %d)",
			ErrBudgetExceeded, b.calls, b.budget.MaxCalls)
	}
	if b.budget.MaxTokens > 0 {
		// In-flight calls hold their worst case so concurrent callers cannot
		// jointly overshoot the budget.
		remaining := b.budget.MaxTokens - b.used.Total() - b.reserved
		if worst > remaining {
			b.mu.Unlock()
			return Response{}, fmt.Errorf("%w: request needs up to %d tokens, %d remain of %d",
				ErrBudgetExceeded, worst, max(remaining, 0), b.budget.MaxTokens)
		}
	}
	// The call counts as soon as it is admitted: a failed call still spent
	// one of the allowed attempts.
	b.calls++
	b.reserved += worst
	b.mu.Unlock()

	resp, err := b.inner.Complete(ctx, req)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved -= worst
	if err != nil {
		return resp, err
	}
	// A refusal is a response, and the provider bills it.
	b.used.InputTokens += resp.Usage.InputTokens
	b.used.OutputTokens += resp.Usage.OutputTokens
	return resp, nil
}

// Used reports calls made and tokens spent so far.
func (b *Budgeted) Used() (calls int, usage Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.used
}

// estimateInputTokens assumes 3 bytes per token. Log-heavy prompts tokenize
// denser than the ~4 bytes of prose, so 4 would under-estimate and let a call
// overspend; erring high only refuses a little early. It stays a pre-check, not
// metering: the provider-reported usage is what gets charged.
func estimateInputTokens(req Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
	}
	return (n + 2) / 3
}
