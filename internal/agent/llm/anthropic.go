package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/davidsugianto/upsera-agent/pkg/redact"
)

const (
	anthropicDefaultMaxTokens = 2048
	// A hostile or broken proxy could return a huge error body; the status and
	// type carry the diagnosis, so the message is only a hint. Counted in runes
	// so a multi-byte character is never split.
	anthropicMaxErrMessage = 300
)

// AnthropicOption configures NewAnthropic (for example a test base URL).
type AnthropicOption func(*anthropicConfig)

type anthropicConfig struct {
	baseURL    string
	maxRetries *int
}

// WithAnthropicBaseURL points the adapter at another endpoint, for tests.
func WithAnthropicBaseURL(u string) AnthropicOption {
	return func(c *anthropicConfig) { c.baseURL = u }
}

// WithAnthropicMaxRetries overrides the SDK's automatic retry count for
// transient errors (429, 5xx, connection failures). Tests set it to 0 to avoid
// backoff delays.
func WithAnthropicMaxRetries(n int) AnthropicOption {
	return func(c *anthropicConfig) { c.maxRetries = &n }
}

type anthropicClient struct {
	c anthropic.Client
}

// NewAnthropic returns a Client backed by the official Anthropic Go SDK. The
// SDK stays inside this package so ADR 0003's vendor boundary holds. An empty
// apiKey leaves credentials to the SDK's default chain (ANTHROPIC_AUTH_TOKEN,
// then a profile from `ant auth login`).
func NewAnthropic(apiKey string, opts ...AnthropicOption) Client {
	var cfg anthropicConfig
	for _, o := range opts {
		o(&cfg)
	}
	var ro []option.RequestOption
	if apiKey != "" {
		ro = append(ro, option.WithAPIKey(apiKey))
	}
	if cfg.baseURL != "" {
		ro = append(ro, option.WithBaseURL(cfg.baseURL))
	}
	if cfg.maxRetries != nil {
		ro = append(ro, option.WithMaxRetries(*cfg.maxRetries))
	}
	return anthropicClient{c: anthropic.NewClient(ro...)}
}

// Complete sends one structured-output request. It deliberately sets no
// sampling parameters, tool_choice, thinking, or prefill: current models
// reject non-default sampling values and forced tools, and thinking stays on
// the model's default.
func (a anthropicClient) Complete(ctx context.Context, req Request) (Response, error) {
	params, err := anthropicParams(req)
	if err != nil {
		return Response{}, err
	}

	msg, err := a.c.Messages.New(ctx, params)
	if err != nil {
		return Response{}, anthropicError(err)
	}

	resp := Response{
		Model:      string(msg.Model),
		StopReason: string(msg.StopReason),
		Usage: Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
	}
	if msg.StopReason == anthropic.StopReasonRefusal {
		resp.Refused = true
		resp.RefusalCategory = string(msg.StopDetails.Category)
		return resp, nil
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return Response{}, ErrEmptyOutput
	}
	resp.Text = text.String()
	return resp, nil
}

func anthropicParams(req Request) (anthropic.MessageNewParams, error) {
	var schema map[string]any
	if err := json.Unmarshal(req.Schema, &schema); err != nil {
		return anthropic.MessageNewParams{}, fmt.Errorf("anthropic: invalid response schema: %w", err)
	}

	msgs := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		default:
			return anthropic.MessageNewParams{}, fmt.Errorf("anthropic: unsupported message role %q", m.Role)
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(maxTokens),
		Messages:  msgs,
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortMedium,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	return params, nil
}

// anthropicError reduces an SDK error to the status, the API error type, and a
// short message. The SDK's own error text embeds the request URL and raw body,
// and the request carries the key and the evidence-bearing prompt, so none of
// that is passed on.
func anthropicError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		// Transport and context errors: the SDK wraps the cause, which for
		// cancellation is context.Canceled or context.DeadlineExceeded.
		if ctxErr := contextCause(err); ctxErr != nil {
			return fmt.Errorf("anthropic request: %w", ctxErr)
		}
		return fmt.Errorf("anthropic request failed: %w", err)
	}

	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(apiErr.RawJSON()), &body)

	errType := body.Error.Type
	if errType == "" {
		errType = string(apiErr.Type())
	}
	msg := fmt.Sprintf("anthropic API error: status %d", apiErr.StatusCode)
	if errType != "" {
		msg += " " + errType
	}
	if m := errorMessageSnippet(body.Error.Message); m != "" {
		msg += ": " + m
	}
	return errors.New(msg)
}

// errorMessageSnippet redacts before truncating: cutting first could split a
// secret below the length the redaction patterns recognise and let half of it
// through. API errors sometimes echo request fragments, and the CLI prints this.
func errorMessageSnippet(m string) string {
	s := strings.TrimSpace(redact.Redact(m).Text)
	if r := []rune(s); len(r) > anthropicMaxErrMessage {
		s = string(r[:anthropicMaxErrMessage]) + "..."
	}
	return s
}

func contextCause(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	}
	return nil
}
