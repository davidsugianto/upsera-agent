package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("openai-compat: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("openai-compat: send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// One extra byte tells "exactly at the limit" from "over it".
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Response{}, fmt.Errorf("openai-compat: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, fmt.Errorf("openai-compat: %s: %s", resp.Status, errorSnippet(raw))
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

// errorSnippet redacts before truncating: cutting first could split a secret
// below the length the redaction patterns recognise and let half of it through.
func errorSnippet(body []byte) string {
	s := strings.TrimSpace(redact.Redact(string(body)).Text)
	if r := []rune(s); len(r) > maxErrorBodyChars {
		s = string(r[:maxErrorBodyChars])
	}
	return s
}
