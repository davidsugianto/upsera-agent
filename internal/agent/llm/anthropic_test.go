package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const anthTestKey = "sk-ant-test-secret-key"

var anthTestSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)

func anthTestRequest() Request {
	return Request{
		Model:      "claude-sonnet-5-5",
		System:     "You are a triage assistant.",
		Messages:   []Message{{Role: "user", Content: "why is the pod crashing?"}},
		SchemaName: "summary",
		Schema:     anthTestSchema,
		MaxTokens:  512,
	}
}

func anthOKBody(text string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",` +
		`"content":[{"type":"text","text":` + anthQuote(text) + `}],` +
		`"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":120,"output_tokens":34}}`
}

func anthQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// newAnthropicTestClient returns an adapter pointed at h with retries off so error
// paths do not sleep.
func newAnthropicTestClient(t *testing.T, h http.HandlerFunc) Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewAnthropic(anthTestKey, WithAnthropicBaseURL(srv.URL), WithAnthropicMaxRetries(0))
}

func TestAnthropicRequestShape(t *testing.T) {
	var gotPath, gotKey string
	var body map[string]any
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthOKBody(`{"answer":"oom"}`))
	})

	if _, err := c.Complete(context.Background(), anthTestRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != anthTestKey {
		t.Errorf("x-api-key header not sent")
	}
	if body["model"] != "claude-sonnet-5-5" {
		t.Errorf("model = %v", body["model"])
	}
	if body["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}

	system, _ := body["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != "You are a triage assistant." {
		t.Errorf("system = %v", body["system"])
	}

	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Errorf("role = %v", m["role"])
	}
	blocks := m["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "why is the pod crashing?" {
		t.Errorf("content = %v", m["content"])
	}

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing: %v", body)
	}
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("format.type = %v", format["type"])
	}
	var want any
	_ = json.Unmarshal(anthTestSchema, &want)
	gotSchema, _ := json.Marshal(format["schema"])
	wantSchema, _ := json.Marshal(want)
	if string(gotSchema) != string(wantSchema) {
		t.Errorf("format.schema = %s, want %s", gotSchema, wantSchema)
	}
	if oc["effort"] != "medium" {
		t.Errorf("effort = %v, want medium", oc["effort"])
	}

	for _, k := range []string{"temperature", "top_p", "top_k", "tool_choice", "thinking", "tools"} {
		if _, present := body[k]; present {
			t.Errorf("request must not set %q", k)
		}
	}
}

func TestAnthropicDefaultMaxTokens(t *testing.T) {
	var body map[string]any
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthOKBody(`{}`))
	})
	req := anthTestRequest()
	req.MaxTokens = 0
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if body["max_tokens"] != float64(2048) {
		t.Errorf("max_tokens = %v, want 2048", body["max_tokens"])
	}
}

func TestAnthropicAssistantTurn(t *testing.T) {
	var body map[string]any
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthOKBody(`{}`))
	})
	req := anthTestRequest()
	req.Messages = []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestAnthropicUnknownRole(t *testing.T) {
	c := newAnthropicTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request must not be sent")
	})
	req := anthTestRequest()
	req.Messages = []Message{{Role: "system", Content: "x"}}
	if _, err := c.Complete(context.Background(), req); err == nil {
		t.Fatal("want error for unsupported role")
	}
}

func TestAnthropicInvalidSchema(t *testing.T) {
	c := newAnthropicTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request must not be sent")
	})
	req := anthTestRequest()
	req.Schema = json.RawMessage(`not json`)
	if _, err := c.Complete(context.Background(), req); err == nil {
		t.Fatal("want error for invalid schema")
	}
}

func TestAnthropicSuccess(t *testing.T) {
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5-20261001",`+
			`"content":[{"type":"text","text":"{\"answer\":"},{"type":"text","text":"\"oom\"}"}],`+
			`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":34}}`)
	})
	resp, err := c.Complete(context.Background(), anthTestRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"answer":"oom"}` {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Usage != (Usage{InputTokens: 120, OutputTokens: 34}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if resp.Model != "claude-sonnet-5-5-20261001" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if resp.Refused {
		t.Error("Refused = true")
	}
}

func TestAnthropicRefusal(t *testing.T) {
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
			`"content":[],"stop_reason":"refusal","stop_sequence":null,`+
			`"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},`+
			`"usage":{"input_tokens":50,"output_tokens":0}}`)
	})
	resp, err := c.Complete(context.Background(), anthTestRequest())
	if err != nil {
		t.Fatalf("a refusal is not an error, got %v", err)
	}
	if !resp.Refused || resp.RefusalCategory != "cyber" {
		t.Errorf("Refused=%v category=%q", resp.Refused, resp.RefusalCategory)
	}
	if resp.Text != "" {
		t.Errorf("Text = %q, want empty", resp.Text)
	}
	if resp.StopReason != "refusal" || resp.Usage.InputTokens != 50 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestAnthropicRefusalWithoutCategory(t *testing.T) {
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
			`"content":[],"stop_reason":"refusal","stop_sequence":null,`+
			`"stop_details":null,"usage":{"input_tokens":5,"output_tokens":0}}`)
	})
	resp, err := c.Complete(context.Background(), anthTestRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Refused || resp.RefusalCategory != "" {
		t.Errorf("Refused=%v category=%q", resp.Refused, resp.RefusalCategory)
	}
}

func TestAnthropicEmptyOutput(t *testing.T) {
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_3","type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
			`"content":[],"stop_reason":"end_turn","stop_sequence":null,`+
			`"usage":{"input_tokens":10,"output_tokens":0}}`)
	})
	_, err := c.Complete(context.Background(), anthTestRequest())
	if !errors.Is(err, ErrEmptyOutput) {
		t.Fatalf("err = %v, want ErrEmptyOutput", err)
	}
}

func TestAnthropicHTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		errType    string
		message    string
		wantStatus string
	}{
		{"rate limited", 429, "rate_limit_error", "slow down", "429"},
		{"server error", 500, "api_error", "internal failure", "500"},
		{"bad request", 400, "invalid_request_error", "temperature is not supported", "400"},
		{"unauthorized", 401, "authentication_error", "invalid credentials", "401"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"`+tc.errType+`","message":"`+tc.message+`"}}`)
			})
			_, err := c.Complete(context.Background(), anthTestRequest())
			if err == nil {
				t.Fatal("want error")
			}
			msg := err.Error()
			for _, want := range []string{tc.wantStatus, tc.errType, tc.message} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			for _, secret := range []string{anthTestKey, "why is the pod crashing?", "x-api-key"} {
				if strings.Contains(msg, secret) {
					t.Errorf("error %q leaks %q", msg, secret)
				}
			}
		})
	}
}

func TestAnthropicErrorWithNonJSONBody(t *testing.T) {
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	_, err := c.Complete(context.Background(), anthTestRequest())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want one mentioning 502", err)
	}
}

func TestAnthropicRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		_, _ = io.WriteString(w, anthOKBody(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := NewAnthropic(anthTestKey, WithAnthropicBaseURL(srv.URL), WithAnthropicMaxRetries(1))
	if _, err := c.Complete(context.Background(), anthTestRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestAnthropicContextCancel(t *testing.T) {
	release := make(chan struct{})
	c := newAnthropicTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Complete(ctx, anthTestRequest())
	if err == nil {
		t.Fatal("want error on cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), anthTestKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancel was not respected promptly")
	}
}

func TestAnthropicErrorMessageIsRedacted(t *testing.T) {
	const bearer = "abcdef0123456789tokenvalue"
	const pair = "hunter2hunter2"
	const email = "ops.person@example.com"
	msg := "bad header Authorization: Bearer " + bearer + " and api_key=" + pair + " from " + email
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":`+anthQuote(msg)+`}}`)
	})
	_, err := c.Complete(context.Background(), anthTestRequest())
	if err == nil {
		t.Fatal("want error")
	}
	for _, secret := range []string{bearer, pair, email} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks %q", err.Error(), secret)
		}
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_request_error") {
		t.Errorf("error %q lost the status or type", err.Error())
	}
}

func TestAnthropicErrorMessageCutOnRuneBoundary(t *testing.T) {
	// Three-byte runes: a byte cap at 300 would split the 101st rune.
	msg := strings.Repeat("世", 400)
	c := newAnthropicTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":`+anthQuote(msg)+`}}`)
	})
	_, err := c.Complete(context.Background(), anthTestRequest())
	if err == nil {
		t.Fatal("want error")
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("error is not valid UTF-8: %q", err.Error())
	}
	if n := strings.Count(err.Error(), "世"); n != 300 {
		t.Errorf("kept %d runes of the message, want 300", n)
	}
}
