package llm_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/agent/llm/llmtest"
)

func okResp(in, out int) llm.Response {
	return llm.Response{Text: "{}", Usage: llm.Usage{InputTokens: in, OutputTokens: out}}
}

func TestBudgetedCallCap(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{okResp(1, 1), okResp(1, 1), okResp(1, 1)}}
	b := llm.NewBudgeted(fake, llm.Budget{MaxCalls: 2})

	for i := 0; i < 2; i++ {
		if _, err := b.Complete(context.Background(), llm.Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := b.Complete(context.Background(), llm.Request{})
	if !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("third call error = %v, want ErrBudgetExceeded", err)
	}
	if len(fake.Requests) != 2 {
		t.Errorf("inner saw %d requests, want 2", len(fake.Requests))
	}
}

func TestBudgetedTokenPrecheckSkipsInner(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{okResp(1, 1)}}
	b := llm.NewBudgeted(fake, llm.Budget{MaxTokens: 100})

	// 40 bytes of system -> 14 input tokens estimated; 14 + 95 > 100.
	req := llm.Request{System: strings.Repeat("a", 40), MaxTokens: 95}
	_, err := b.Complete(context.Background(), req)
	if !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if len(fake.Requests) != 0 {
		t.Errorf("inner was called %d times, want 0", len(fake.Requests))
	}
	if calls, _ := b.Used(); calls != 0 {
		t.Errorf("refused pre-check counted %d calls, want 0", calls)
	}
}

func TestBudgetedTokenEstimateRoundsUpAndSumsMessages(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{okResp(1, 1)}}
	// 1 (system) + 2 + 2 (messages) = 5 bytes -> ceil(5/3) = 2 tokens.
	req := llm.Request{
		System:    "a",
		Messages:  []llm.Message{{Role: "user", Content: "bb"}, {Role: "assistant", Content: "cc"}},
		MaxTokens: 9,
	}
	// 2 + 9 = 11 > 10 -> refused.
	b := llm.NewBudgeted(fake, llm.Budget{MaxTokens: 10})
	if _, err := b.Complete(context.Background(), req); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	// 2 + 8 = 10 fits exactly.
	req.MaxTokens = 8
	if _, err := b.Complete(context.Background(), req); err != nil {
		t.Fatalf("exact fit refused: %v", err)
	}
}

// Log-heavy prompts tokenize near 3 bytes per token, so the estimate must not
// assume 4: this request would fit at 4 bytes/token (3 + 7 = 10) but not at 3
// (4 + 7 = 11).
func TestBudgetedEstimateIsConservativeForDenseText(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{okResp(1, 1)}}
	b := llm.NewBudgeted(fake, llm.Budget{MaxTokens: 10})

	req := llm.Request{System: strings.Repeat("x", 12), MaxTokens: 7}
	if _, err := b.Complete(context.Background(), req); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if len(fake.Requests) != 0 {
		t.Errorf("inner was called %d times, want 0", len(fake.Requests))
	}
}

func TestBudgetedAccumulatesUsageAndShrinksRemaining(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{okResp(30, 20), okResp(5, 5)}}
	b := llm.NewBudgeted(fake, llm.Budget{MaxTokens: 100})

	if _, err := b.Complete(context.Background(), llm.Request{MaxTokens: 50}); err != nil {
		t.Fatal(err)
	}
	// 50 used, 50 remaining: a 51-token request must be refused.
	if _, err := b.Complete(context.Background(), llm.Request{MaxTokens: 51}); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if _, err := b.Complete(context.Background(), llm.Request{MaxTokens: 50}); err != nil {
		t.Fatal(err)
	}
	calls, u := b.Used()
	if calls != 2 || u != (llm.Usage{InputTokens: 35, OutputTokens: 25}) {
		t.Errorf("Used() = %d, %+v; want 2, {35 25}", calls, u)
	}
}

func TestBudgetedUnlimitedWhenZero(t *testing.T) {
	fake := &llmtest.Fake{Responses: []llm.Response{okResp(1_000_000, 1_000_000), okResp(1, 1)}}
	b := llm.NewBudgeted(fake, llm.Budget{})
	for i := 0; i < 2; i++ {
		if _, err := b.Complete(context.Background(), llm.Request{MaxTokens: 1 << 30}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestBudgetedRefusalStillCountsUsage(t *testing.T) {
	refused := llm.Response{Refused: true, RefusalCategory: "cyber", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}}
	b := llm.NewBudgeted(&llmtest.Fake{Responses: []llm.Response{refused}}, llm.Budget{})

	resp, err := b.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("refusal should not be an error: %v", err)
	}
	if !resp.Refused {
		t.Error("Refused flag lost")
	}
	if _, u := b.Used(); u.Total() != 10 {
		t.Errorf("usage total = %d, want 10", u.Total())
	}
}

func TestBudgetedInnerErrorCountsCallNotTokens(t *testing.T) {
	boom := errors.New("boom")
	fake := &llmtest.Fake{
		Responses: []llm.Response{{Usage: llm.Usage{InputTokens: 99, OutputTokens: 99}}},
		Errs:      []error{boom},
	}
	b := llm.NewBudgeted(fake, llm.Budget{MaxCalls: 1})

	_, err := b.Complete(context.Background(), llm.Request{})
	if err != boom { //nolint:errorlint // must be returned unchanged, not wrapped
		t.Fatalf("error = %v, want the inner error unchanged", err)
	}
	calls, u := b.Used()
	if calls != 1 || u.Total() != 0 {
		t.Errorf("Used() = %d, %+v; want 1 call, 0 tokens", calls, u)
	}
	if _, err := b.Complete(context.Background(), llm.Request{}); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Errorf("failed call must still consume the call budget, got %v", err)
	}
}

func TestBudgetedConcurrent(t *testing.T) {
	const n = 50
	resps := make([]llm.Response, n)
	for i := range resps {
		resps[i] = okResp(2, 3)
	}
	b := llm.NewBudgeted(&llmtest.Fake{Responses: resps}, llm.Budget{MaxCalls: 20})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var okCount, refused int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Complete(context.Background(), llm.Request{})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
			} else if errors.Is(err, llm.ErrBudgetExceeded) {
				refused++
			}
		}()
	}
	wg.Wait()

	if okCount != 20 || refused != n-20 {
		t.Errorf("ok=%d refused=%d, want 20 and %d", okCount, refused, n-20)
	}
	calls, u := b.Used()
	if calls != 20 || u != (llm.Usage{InputTokens: 40, OutputTokens: 60}) {
		t.Errorf("Used() = %d, %+v; want 20, {40 60}", calls, u)
	}
}
