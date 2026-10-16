// Package mcphandler serves the agent's read-only tools over the Model Context
// Protocol, so the same tools work from Claude Code or any MCP client (INT-4).
//
// Only non-mutating tools are exposed, and a call returns only the tool's
// Summary, never its Data. Output is redacted and size-capped by the tools,
// but it is NOT wrapped in untrusted-data framing as it is for the agent's own
// LLM input, so MCP clients must treat it as untrusted data: cluster objects
// and logs can carry text written by anyone able to create them.
//
// Nothing in this package writes to stdout, which is the protocol channel for
// the stdio transport; diagnostics go to the supplied logger.
package mcphandler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	"github.com/davidsugianto/upsera-agent/pkg/redact"
)

// NewServer returns an MCP server exposing every non-mutating tool in reg.
// Diagnostics go to log, never to stdout, which is the protocol channel.
func NewServer(reg *tools.Registry, version string, log *slog.Logger) *sdk.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := sdk.NewServer(
		&sdk.Implementation{Name: "upsera-agent", Version: version},
		&sdk.ServerOptions{Logger: log},
	)
	if reg == nil {
		return srv
	}

	names := reg.Names()
	sort.Strings(names)
	for _, name := range names {
		t, ok := reg.Get(name)
		// Defence in depth: the registry should already have dropped mutating
		// tools, but this server never exposes one regardless of how reg was
		// built.
		if !ok || t.Mutating() {
			continue
		}
		// The raw Server.AddTool does no argument unmarshalling or schema
		// validation; tools.Tool.Call decodes strictly itself. AddTool panics
		// if the schema is not a JSON object of type "object".
		srv.AddTool(&sdk.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: json.RawMessage(t.Schema()),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		}, newHandler(t, log))
	}
	return srv
}

// maxErrorBytes caps the error text returned to a client, so a call cannot make
// the server echo back a large client-supplied string.
const maxErrorBytes = 2 << 10

const truncMarker = "[truncated]"

// capErrorText shortens s to maxErrorBytes on a UTF-8 rune boundary and appends
// truncMarker. Callers must redact first, so a secret cut in half cannot slip
// past a redaction pattern.
func capErrorText(s string) string {
	if len(s) <= maxErrorBytes {
		return s
	}
	i := maxErrorBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "\n" + truncMarker
}

// newHandler adapts a tools.Tool to an MCP tool handler. Tool failures become
// IsError results rather than Go errors, so one bad call never ends the
// session; only context cancellation is returned as an error.
func newHandler(t tools.Tool, log *slog.Logger) sdk.ToolHandler {
	name := t.Name()
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args []byte
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}

		start := time.Now()
		res, err := t.Call(ctx, args)
		dur := time.Since(start)

		if err != nil {
			// Never log argument values or summaries; the error text is not
			// logged either, since tool errors may quote input.
			log.LogAttrs(ctx, slog.LevelWarn, "mcp tool call",
				slog.String("tool", name),
				slog.Duration("duration", dur),
				slog.Bool("ok", false),
			)
			if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil, ctxErr
			}
			// Tool errors can echo object names or API server messages, so
			// redact them like successful summaries.
			return &sdk.CallToolResult{
				IsError: true,
				Content: []sdk.Content{&sdk.TextContent{Text: capErrorText(redact.Redact(err.Error()).Text)}},
			}, nil
		}

		log.LogAttrs(ctx, slog.LevelInfo, "mcp tool call",
			slog.String("tool", name),
			slog.Duration("duration", dur),
			slog.Bool("ok", true),
			slog.Bool("truncated", res.Truncated),
			slog.Any("redactions", res.Redactions),
		)
		// Only Summary leaves the process; Data stays behind (see tools.Result).
		// A truncated Summary already carries its marker, so nothing is added.
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: res.Summary}},
		}, nil
	}
}
