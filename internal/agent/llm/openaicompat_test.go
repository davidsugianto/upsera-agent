package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sk-test-abcdefghijklmnopqrstuvwxyz0123"

func testRequest() Request {
	return Request{
		Model:      "gpt-test",
		System:     "be terse",
		Messages:   []Message{{Role: "user", Content: "why is it down?"}},
		SchemaName: "summary",
		Schema:     json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		MaxTokens:  512,
	}
}

const okBody = `{"model":"gpt-test-2024","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"a\":\"b\"}"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`

// capture records the last request a fake server saw.
type capture struct {
	path   string
	header http.Header
	body   map[string]json.RawMessage
}

func newServer(t *testing.T, status int, reply string) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path = r.URL.Path
		c.header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &c.body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestOpenAICompatRequestShape(t *testing.T) {
	srv, c := newServer(t, http.StatusOK, okBody)
	cl := NewOpenAICompat(srv.URL+"/v1", testKey, nil)

	if _, err := cl.Complete(context.Background(), testRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if c.path != "/v1/chat/completions" {
		t.Errorf("path = %q", c.path)
	}
	if got := c.header.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("Authorization = %q", got)
	}
	if got := c.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	for _, banned := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := c.body[banned]; ok {
			t.Errorf("body must not set %s", banned)
		}
	}

	var model string
	_ = json.Unmarshal(c.body["model"], &model)
	if model != "gpt-test" {
		t.Errorf("model = %q", model)
	}
	var maxTokens int
	_ = json.Unmarshal(c.body["max_tokens"], &maxTokens)
	if maxTokens != 512 {
		t.Errorf("max_tokens = %d", maxTokens)
	}

	var msgs []struct{ Role, Content string }
	if err := json.Unmarshal(c.body["messages"], &msgs); err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != "be terse" ||
		msgs[1].Role != "user" || msgs[1].Content != "why is it down?" {
		t.Errorf("messages = %+v", msgs)
	}

	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(c.body["response_format"], &rf); err != nil {
		t.Fatalf("response_format: %v", err)
	}
	if rf.Type != "json_schema" || rf.JSONSchema.Name != "summary" || !rf.JSONSchema.Strict {
		t.Errorf("response_format = %+v", rf)
	}
	if !strings.Contains(string(rf.JSONSchema.Schema), `"properties"`) {
		t.Errorf("schema not forwarded: %s", rf.JSONSchema.Schema)
	}
}

func TestOpenAICompatNoAuthHeaderWithoutKey(t *testing.T) {
	srv, c := newServer(t, http.StatusOK, okBody)
	cl := NewOpenAICompat(srv.URL, "", nil)

	if _, err := cl.Complete(context.Background(), testRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := c.header["Authorization"]; ok {
		t.Errorf("Authorization must be absent, got %q", c.header.Get("Authorization"))
	}
}

func TestOpenAICompatDefaultMaxTokensAndNoSystem(t *testing.T) {
	srv, c := newServer(t, http.StatusOK, okBody)
	cl := NewOpenAICompat(srv.URL, "", nil)
	req := testRequest()
	req.MaxTokens = 0
	req.System = ""

	if _, err := cl.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var maxTokens int
	_ = json.Unmarshal(c.body["max_tokens"], &maxTokens)
	if maxTokens != 2048 {
		t.Errorf("max_tokens = %d, want 2048", maxTokens)
	}
	var msgs []struct{ Role string }
	_ = json.Unmarshal(c.body["messages"], &msgs)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Errorf("empty System must not add a system message: %+v", msgs)
	}
}

func TestOpenAICompatTrailingSlash(t *testing.T) {
	srv, c := newServer(t, http.StatusOK, okBody)
	cl := NewOpenAICompat(srv.URL+"/v1/", "", nil)

	if _, err := cl.Complete(context.Background(), testRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if c.path != "/v1/chat/completions" {
		t.Errorf("path = %q", c.path)
	}
}

func TestOpenAICompatSuccess(t *testing.T) {
	srv, _ := newServer(t, http.StatusOK, okBody)
	cl := NewOpenAICompat(srv.URL, "", nil)

	got, err := cl.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := Response{
		Text:       `{"a":"b"}`,
		Usage:      Usage{InputTokens: 11, OutputTokens: 7},
		Model:      "gpt-test-2024",
		StopReason: "stop",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestOpenAICompatLengthFinishReason(t *testing.T) {
	body := `{"model":"m","choices":[{"finish_reason":"length","message":{"content":"{\"a\":"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	srv, _ := newServer(t, http.StatusOK, body)
	cl := NewOpenAICompat(srv.URL, "", nil)

	got, err := cl.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.StopReason != "length" || got.Text != `{"a":` || got.Refused {
		t.Errorf("got %+v", got)
	}
}

func TestOpenAICompatRefusal(t *testing.T) {
	body := `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"partial","refusal":"I can't help with that."}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	srv, _ := newServer(t, http.StatusOK, body)
	cl := NewOpenAICompat(srv.URL, "", nil)

	got, err := cl.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("a refusal is not an error: %v", err)
	}
	if !got.Refused || got.Text != "" {
		t.Errorf("got %+v, want Refused with empty Text", got)
	}
	if got.Usage.Total() != 7 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func TestOpenAICompatEmptyOutput(t *testing.T) {
	for name, body := range map[string]string{
		"no choices":    `{"model":"m","choices":[]}`,
		"empty content": `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":""}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := newServer(t, http.StatusOK, body)
			cl := NewOpenAICompat(srv.URL, "", nil)

			_, err := cl.Complete(context.Background(), testRequest())
			if !errors.Is(err, ErrEmptyOutput) {
				t.Errorf("err = %v, want ErrEmptyOutput", err)
			}
		})
	}
}

func TestOpenAICompatHTTPErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// A misbehaving proxy may echo the credential back.
			srv, _ := newServer(t, status, `{"error":"bad key `+testKey+`"}`)
			cl := NewOpenAICompat(srv.URL, testKey, nil)

			_, err := cl.Complete(context.Background(), testRequest())
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, http.StatusText(status)) && !strings.Contains(msg, strconv.Itoa(status)) {
				t.Errorf("error lacks status %d: %v", status, msg)
			}
			if strings.Contains(msg, testKey) || strings.Contains(msg, "Bearer") {
				t.Errorf("error leaks the credential: %v", msg)
			}
		})
	}
}

func TestOpenAICompatErrorBodyTruncated(t *testing.T) {
	srv, _ := newServer(t, http.StatusBadGateway, strings.Repeat("x", 5000))
	cl := NewOpenAICompat(srv.URL, "", nil)

	_, err := cl.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := strings.Count(err.Error(), "x"); n > 300 {
		t.Errorf("error carries %d body chars, want <= 300", n)
	}
}

func TestOpenAICompatContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	cl := NewOpenAICompat(srv.URL, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cl.Complete(ctx, testRequest())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Complete did not return promptly after cancel")
	}
}

// countingTransport counts the response bytes the caller actually consumed.
type countingTransport struct {
	read atomic.Int64
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		resp.Body = countingBody{resp.Body, &c.read}
	}
	return resp, err
}

func TestOpenAICompatOversizedBodyIsBounded(t *testing.T) {
	const limit = 4 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"`)
		chunk := []byte(strings.Repeat("a", 64<<10))
		for i := 0; i < 160; i++ { // ~10 MiB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	ct := &countingTransport{}
	cl := NewOpenAICompat(srv.URL, "", &http.Client{Transport: ct})

	_, err := cl.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected an error for an oversized body")
	}
	if got := ct.read.Load(); got > limit+1 {
		t.Errorf("read %d bytes, want <= %d", got, limit+1)
	}
}

func TestOpenAICompatMalformedJSON(t *testing.T) {
	srv, _ := newServer(t, http.StatusOK, `not json`)
	cl := NewOpenAICompat(srv.URL, "", nil)

	if _, err := cl.Complete(context.Background(), testRequest()); err == nil {
		t.Fatal("expected a decode error")
	}
}

// oaiStubTransport answers every request from memory and records its body, so
// a test can use a real api.openai.com base URL without any network.
type oaiStubTransport struct {
	body map[string]json.RawMessage
}

func (s *oaiStubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	s.body = nil
	_ = json.Unmarshal(raw, &s.body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(okBody)),
		Request:    r,
	}, nil
}

func TestOpenAICompatTokenLimitFieldDependsOnHost(t *testing.T) {
	tests := []struct {
		baseURL string
		want    string // field that must carry the limit
		other   string // field that must be absent
	}{
		{"https://api.openai.com/v1", "max_completion_tokens", "max_tokens"},
		{"https://api.openai.com/v1/", "max_completion_tokens", "max_tokens"},
		{"https://API.OpenAI.com/v1", "max_completion_tokens", "max_tokens"},
		{"https://api.openai.com.evil.example/v1", "max_tokens", "max_completion_tokens"},
		{"https://evil.example/api.openai.com/v1", "max_tokens", "max_completion_tokens"},
		{"https://notapi.openai.com/v1", "max_tokens", "max_completion_tokens"},
		{"http://localhost:11434/v1", "max_tokens", "max_completion_tokens"},
		{"http://vllm.internal:8000/v1", "max_tokens", "max_completion_tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.baseURL, func(t *testing.T) {
			stub := &oaiStubTransport{}
			cl := NewOpenAICompat(tt.baseURL, testKey, &http.Client{Transport: stub})

			if _, err := cl.Complete(context.Background(), testRequest()); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			var got int
			if err := json.Unmarshal(stub.body[tt.want], &got); err != nil || got != 512 {
				t.Errorf("%s = %s, want 512", tt.want, stub.body[tt.want])
			}
			if _, ok := stub.body[tt.other]; ok {
				t.Errorf("%s must be omitted", tt.other)
			}
		})
	}
}

func TestOpenAICompatDefaultTokenLimitOnOpenAIHost(t *testing.T) {
	stub := &oaiStubTransport{}
	cl := NewOpenAICompat("https://api.openai.com/v1", testKey, &http.Client{Transport: stub})
	req := testRequest()
	req.MaxTokens = 0

	if _, err := cl.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var got int
	_ = json.Unmarshal(stub.body["max_completion_tokens"], &got)
	if got != 2048 {
		t.Errorf("max_completion_tokens = %d, want 2048", got)
	}
}
