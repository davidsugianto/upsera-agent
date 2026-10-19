package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/davidsugianto/upsera-agent/pkg/redact"
)

const (
	defaultMaxTokens = 2048
	// A reply is one JSON summary; a body past this is a misbehaving endpoint,
	// not a long answer.
	maxResponseBytes = 4 << 20
	// Enough to see why a call failed without flooding logs or terminals.
	maxErrorBodyChars  = 300
	defaultHTTPTimeout = 120 * time.Second
)

// A rate-limited call (429) is retried, because a provider's per-minute token
// limit clears on its own and one throttled call should not fail a whole
// investigation. Any other failure is returned at once.
//
// Retries happen inside one Complete call, so the call budget (Budgeted) counts
// the logical call once. A throttled attempt normally spends no tokens, and the
// caller's context bounds the total time.
const (
	maxRateLimitRetries = 3
	baseRetryWait       = time.Second
	// A server-requested wait shorter than this is raised to it, so a tiny
	// Retry-After cannot turn the retries into a tight loop.
	minRetryWait = 100 * time.Millisecond
	// A server-requested wait longer than this is a quota problem, not a
	// burst: give up instead of retrying.
	maxRetryWait = 30 * time.Second
)

// retrySleep waits between attempts. A variable so tests do not really sleep.
var retrySleep = sleepCtx

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// openAIHost is the one endpoint that takes max_completion_tokens.
const openAIHost = "api.openai.com"

type openAICompat struct {
	endpoint string
	apiKey   string
	http     *http.Client
	// completionTokens selects max_completion_tokens over max_tokens: OpenAI's
	// newer models reject max_tokens, while Ollama, vLLM and similar servers
	// only understand it.
	completionTokens bool
}

// NewOpenAICompat returns a Client for any OpenAI-compatible chat completions
// endpoint (OpenAI, Ollama, vLLM). baseURL ends before /chat/completions.
// A nil httpClient means a client with a 120 s timeout.
func NewOpenAICompat(baseURL, apiKey string, httpClient *http.Client) Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &openAICompat{
		endpoint:         strings.TrimRight(baseURL, "/") + "/chat/completions",
		apiKey:           apiKey,
		http:             httpClient,
		completionTokens: isOpenAIHost(baseURL),
	}
}

// isOpenAIHost compares the parsed host exactly, so api.openai.com.evil.example
// or a path containing the name does not switch the request shape.
func isOpenAIHost(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && strings.EqualFold(u.Hostname(), openAIHost)
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type oaiResponseFormat struct {
	Type       string        `json:"type"`
	JSONSchema oaiJSONSchema `json:"json_schema"`
}

// The request type has no sampling fields on purpose: several current models
// reject non-default values, so they must not be expressible here.
type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	// Exactly one of the two limits is set (omitempty drops the zero one).
	MaxTokens           int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens int               `json:"max_completion_tokens,omitempty"`
	ResponseFormat      oaiResponseFormat `json:"response_format"`
}

type oaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string  `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (c *openAICompat) Complete(ctx context.Context, req Request) (Response, error) {
	msgs := make([]oaiMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, oaiMessage(m))
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	body := oaiRequest{
		Model:    req.Model,
		Messages: msgs,
		ResponseFormat: oaiResponseFormat{
			Type:       "json_schema",
			JSONSchema: oaiJSONSchema{Name: req.SchemaName, Schema: req.Schema, Strict: true},
		},
	}
	if c.completionTokens {
		body.MaxCompletionTokens = maxTokens
	} else {
		body.MaxTokens = maxTokens
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("openai-compat: encode request: %w", err)
	}

	var raw []byte
	for attempt := 0; ; attempt++ {
		status, header, body, err := c.post(ctx, payload)
		if err != nil {
			return Response{}, err
		}
		if status == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
			if wait, ok := retryWait(header, attempt); ok {
				if err := retrySleep(ctx, wait); err != nil {
					return Response{}, fmt.Errorf("openai-compat: waiting to retry after a 429: %w", err)
				}
				continue
			}
		}
		if status < 200 || status > 299 {
			retried := ""
			if attempt > 0 {
				retried = fmt.Sprintf(" after %d retries", attempt)
			}
			return Response{}, fmt.Errorf("openai-compat: %d %s%s: %s", status, http.StatusText(status), retried, errorSnippet(body))
		}
		raw = body
		break
	}
	if len(raw) > maxResponseBytes {
		return Response{}, fmt.Errorf("openai-compat: response exceeds %d bytes", maxResponseBytes)
	}

	var out oaiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("openai-compat: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, ErrEmptyOutput
	}

	choice := out.Choices[0]
	got := Response{
		Usage:      Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
		Model:      out.Model,
		StopReason: choice.FinishReason,
	}
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		got.Refused = true
		return got, nil
	}
	if choice.Message.Content == "" {
		return Response{}, ErrEmptyOutput
	}
	got.Text = choice.Message.Content
	return got, nil
}

// post sends one attempt and returns the status, headers and a size-bounded
// body. The request is rebuilt each time because a body reader is single use.
func (c *openAICompat) post(ctx context.Context, payload []byte) (int, http.Header, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai-compat: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai-compat: send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// One extra byte tells "exactly at the limit" from "over it".
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai-compat: read response: %w", err)
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// retryWait is how long to wait before retry number attempt+1, and whether to
// retry at all. A usable Retry-After (seconds, above zero) is obeyed, but one
// longer than maxRetryWait means a quota rather than a burst, so there is no
// retry. Without one, back off exponentially from baseRetryWait.
//
// The header is compared as a float before any conversion: converting an
// out-of-range float to a Duration is implementation-defined in Go.
func retryWait(h http.Header, attempt int) (time.Duration, bool) {
	secs, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Retry-After")), 64)
	switch {
	case err != nil || math.IsNaN(secs) || secs <= 0:
		return min(baseRetryWait<<attempt, maxRetryWait), true
	case secs > maxRetryWait.Seconds():
		return 0, false
	default:
		return max(time.Duration(secs*float64(time.Second)), minRetryWait), true
	}
}

// errorSnippet redacts before truncating: cutting first could split a secret
// below the length the redaction patterns recognise and let half of it through.
func errorSnippet(body []byte) string {
	s := strings.TrimSpace(redact.Redact(string(body)).Text)
	if r := []rune(s); len(r) > maxErrorBodyChars {
		s = string(r[:maxErrorBodyChars])
	}
	return s
}
