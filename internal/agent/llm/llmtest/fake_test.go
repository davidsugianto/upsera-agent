package llmtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/agent/llm/llmtest"
)

func TestFakeReplaysInOrderAndRecords(t *testing.T) {
	f := &llmtest.Fake{Responses: []llm.Response{{Text: "one"}, {Text: "two"}}}

	for i, want := range []string{"one", "two"} {
		resp, err := f.Complete(context.Background(), llm.Request{System: want})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if resp.Text != want {
			t.Errorf("call %d text = %q, want %q", i, resp.Text, want)
		}
	}
	if len(f.Requests) != 2 || f.Requests[1].System != "two" {
		t.Errorf("Requests = %+v", f.Requests)
	}
}

func TestFakeRecordsDeepCopy(t *testing.T) {
	f := &llmtest.Fake{Responses: []llm.Response{{}}}
	msgs := []llm.Message{{Role: "user", Content: "original"}}
	schema := json.RawMessage(`{"a":1}`)

	if _, err := f.Complete(context.Background(), llm.Request{Messages: msgs, Schema: schema}); err != nil {
		t.Fatal(err)
	}
	msgs[0].Content = "mutated"
	schema[2] = 'z'

	got := f.Requests[0]
	if got.Messages[0].Content != "original" {
		t.Errorf("recorded messages alias the caller's slice: %q", got.Messages[0].Content)
	}
	if string(got.Schema) != `{"a":1}` {
		t.Errorf("recorded schema aliases the caller's bytes: %s", got.Schema)
	}
}

func TestFakeErrsAlignedByIndex(t *testing.T) {
	boom := errors.New("boom")
	f := &llmtest.Fake{
		Responses: []llm.Response{{Text: "a"}, {Text: "b"}},
		Errs:      []error{nil, boom},
	}
	if _, err := f.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := f.Complete(context.Background(), llm.Request{}); !errors.Is(err, boom) {
		t.Fatalf("second error = %v, want boom", err)
	}
}

func TestFakeErrorsWhenScriptExhausted(t *testing.T) {
	f := &llmtest.Fake{Responses: []llm.Response{{Text: "only"}}}
	if _, err := f.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
	_, err := f.Complete(context.Background(), llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "no scripted response") {
		t.Fatalf("error = %v, want a clear out-of-script message", err)
	}
	if len(f.Requests) != 2 {
		t.Errorf("the over-budget request should still be recorded, got %d", len(f.Requests))
	}
}

func TestFakeConcurrent(t *testing.T) {
	const n = 40
	f := &llmtest.Fake{Responses: make([]llm.Response, n)}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.Complete(context.Background(), llm.Request{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(f.Requests) != n {
		t.Errorf("recorded %d requests, want %d", len(f.Requests), n)
	}
}

func TestJSON(t *testing.T) {
	resp := llmtest.JSON(map[string]any{"k": "v"})
	if resp.Text != `{"k":"v"}` {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("Usage = %+v, want small positive counts", resp.Usage)
	}
	if resp.Refused {
		t.Error("JSON response must not be a refusal")
	}
}

func TestJSONPanicsOnUnmarshalable(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic for a value that cannot be marshaled")
		}
	}()
	llmtest.JSON(make(chan int))
}
