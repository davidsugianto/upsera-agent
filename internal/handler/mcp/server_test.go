package mcphandler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	k8stools "github.com/davidsugianto/upsera-agent/internal/agent/tools/k8s"
	"github.com/davidsugianto/upsera-agent/internal/config"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
	"github.com/davidsugianto/upsera-agent/pkg/redact"
)

const (
	secretData = "DATA-MUST-NOT-LEAK-7f3a"
	argSecret  = "ARG-VALUE-MUST-NOT-BE-LOGGED-91c2"
	sumSecret  = "SUMMARY-MUST-NOT-BE-LOGGED-55d0"
)

const objSchema = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`

// fakeTool decodes strictly, like the real tools, and returns a fixed result.
type fakeTool struct {
	name     string
	mutating bool
	res      tools.Result
	err      error
	block    bool // wait for ctx cancellation

	mu    sync.Mutex
	calls int
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "desc of " + f.name }
func (f *fakeTool) Schema() []byte      { return []byte(objSchema) }
func (f *fakeTool) Mutating() bool      { return f.mutating }

func (f *fakeTool) Call(ctx context.Context, args []byte) (tools.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return tools.Result{}, ctx.Err()
	}
	if f.err != nil {
		return tools.Result{}, f.err
	}
	if len(bytes.TrimSpace(args)) == 0 {
		return tools.Result{}, errors.New(f.name + ": invalid arguments: arguments are empty")
	}
	var in struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return tools.Result{}, errors.New(f.name + ": invalid arguments: " + err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return tools.Result{}, errors.New(f.name + ": invalid arguments: trailing data")
	}
	return f.res, nil
}

func (f *fakeTool) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func okResult(summary string) tools.Result {
	return tools.Result{
		Summary:    summary,
		Data:       map[string]string{"secret": secretData},
		Redactions: map[string]int{"token": 2},
	}
}

// connect starts srv over an in-memory transport and returns a client session.
func connect(t *testing.T, srv *sdk.Server) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func textOf(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want exactly 1 content item, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("want *TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func noData(t *testing.T, res *sdk.CallToolResult) {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secretData) {
		t.Fatalf("response leaks Result.Data: %s", b)
	}
	if res.StructuredContent != nil {
		t.Fatalf("unexpected structured content: %v", res.StructuredContent)
	}
}

func TestListToolsOnlyNonMutatingSorted(t *testing.T) {
	reg := tools.NewRegistry(true,
		&fakeTool{name: "zeta"},
		&fakeTool{name: "alpha"},
		&fakeTool{name: "scale_deployment", mutating: true},
		&fakeTool{name: "mid"},
	)
	cs := connect(t, NewServer(reg, "v1.2.3", nil))

	if got := cs.InitializeResult().ServerInfo; got.Name != "upsera-agent" || got.Version != "v1.2.3" {
		t.Fatalf("server info = %+v", got)
	}

	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
		if tl.Description != "desc of "+tl.Name {
			t.Errorf("%s: description %q", tl.Name, tl.Description)
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s: want ReadOnlyHint", tl.Name)
		}
		b, _ := json.Marshal(tl.InputSchema)
		var got, want any
		_ = json.Unmarshal(b, &got)
		_ = json.Unmarshal([]byte(objSchema), &want)
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if !bytes.Equal(gb, wb) {
			t.Errorf("%s: schema %s, want %s", tl.Name, gb, wb)
		}
	}
	if strings.Join(names, ",") != "alpha,mid,zeta" {
		t.Fatalf("tools = %v, want [alpha mid zeta]", names)
	}
}

func TestMutatingToolRejectedByName(t *testing.T) {
	mut := &fakeTool{name: "scale_deployment", mutating: true, res: okResult("scaled")}
	reg := tools.NewRegistry(true, mut, &fakeTool{name: "ok", res: okResult("fine")})
	cs := connect(t, NewServer(reg, "v", nil))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "scale_deployment", Arguments: map[string]any{"name": "x"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("calling a mutating tool must be rejected, got %+v", res)
	}
	if mut.callCount() != 0 {
		t.Fatal("mutating tool was invoked")
	}
	// Session still usable.
	if got := textOf(t, call(t, cs, "ok", map[string]any{"name": "x"})); got != "fine" {
		t.Fatalf("follow-up call = %q", got)
	}
}

func TestCallReturnsOnlySummary(t *testing.T) {
	reg := tools.NewRegistry(false, &fakeTool{name: "t", res: okResult("the summary")})
	cs := connect(t, NewServer(reg, "v", nil))

	res := call(t, cs, "t", map[string]any{"name": "x"})
	if res.IsError {
		t.Fatalf("unexpected IsError: %+v", res)
	}
	if got := textOf(t, res); got != "the summary" {
		t.Fatalf("text = %q", got)
	}
	noData(t, res)
}

func TestTruncatedPassesThroughUnchanged(t *testing.T) {
	sum := strings.Repeat("x", 100) + "\n[truncated]"
	r := okResult(sum)
	r.Truncated = true
	reg := tools.NewRegistry(false, &fakeTool{name: "t", res: r})
	cs := connect(t, NewServer(reg, "v", nil))

	res := call(t, cs, "t", map[string]any{"name": "x"})
	if got := textOf(t, res); got != sum {
		t.Fatalf("text changed:\n got %q\nwant %q", got, sum)
	}
	noData(t, res)
}

func TestBadArgsIsErrorAndSessionSurvives(t *testing.T) {
	reg := tools.NewRegistry(false, &fakeTool{name: "t", res: okResult("good")})
	cs := connect(t, NewServer(reg, "v", nil))

	for _, args := range []any{
		map[string]any{"name": "x", "unknown": 1},
		map[string]any{"name": 7},
	} {
		res := call(t, cs, "t", args)
		if !res.IsError {
			t.Fatalf("args %v: want IsError, got %+v", args, res)
		}
		if txt := textOf(t, res); !strings.Contains(txt, "invalid arguments") {
			t.Fatalf("args %v: text %q", args, txt)
		}
		noData(t, res)
	}
	if got := textOf(t, call(t, cs, "t", map[string]any{"name": "x"})); got != "good" {
		t.Fatalf("follow-up = %q", got)
	}

	// Empty arguments are passed through as-is and the tool rejects them.
	ft := &fakeTool{name: "t", res: okResult("good")}
	res, err := newHandler(ft, slog.New(slog.DiscardHandler))(context.Background(),
		&sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: "t"}})
	if err != nil || !res.IsError || !strings.Contains(textOf(t, res), "arguments are empty") {
		t.Fatalf("nil args: res=%+v err=%v", res, err)
	}
}

func TestToolErrorIsError(t *testing.T) {
	reg := tools.NewRegistry(false,
		&fakeTool{name: "broken", err: errors.New("broken: cluster unreachable")},
		&fakeTool{name: "ok", res: okResult("fine")},
	)
	cs := connect(t, NewServer(reg, "v", nil))

	res := call(t, cs, "broken", map[string]any{"name": "x"})
	if !res.IsError {
		t.Fatalf("want IsError, got %+v", res)
	}
	if txt := textOf(t, res); !strings.Contains(txt, "broken: cluster unreachable") {
		t.Fatalf("text %q", txt)
	}
	if got := textOf(t, call(t, cs, "ok", map[string]any{"name": "x"})); got != "fine" {
		t.Fatalf("follow-up = %q", got)
	}
}

func TestToolErrorTextIsRedacted(t *testing.T) {
	const tokenSecret = "abc123.def-456_xyz"
	var buf syncBuf
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg := tools.NewRegistry(false,
		&fakeTool{name: "leaky", err: errors.New("leaky: api said Authorization: Bearer " + tokenSecret)},
	)
	cs := connect(t, NewServer(reg, "v", log))

	res := call(t, cs, "leaky", map[string]any{"name": "x"})
	if !res.IsError {
		t.Fatalf("want IsError, got %+v", res)
	}
	txt := textOf(t, res)
	if strings.Contains(txt, tokenSecret) {
		t.Fatalf("error text leaks secret: %q", txt)
	}
	if !strings.HasPrefix(txt, "leaky:") {
		t.Fatalf("error text lost tool name prefix: %q", txt)
	}
	if !strings.Contains(txt, redact.Marker) {
		t.Fatalf("error text missing redaction marker: %q", txt)
	}
	if out := buf.String(); strings.Contains(out, tokenSecret) {
		t.Fatalf("log leaks secret:\n%s", out)
	}
}

func TestToolErrorTextIsCapped(t *testing.T) {
	const (
		limit  = 2 << 10
		marker = "[truncated]"
	)
	// "big: " is 5 bytes; pad so a 3-byte rune straddles the cut at limit.
	prefix := "big: " + strings.Repeat("a", limit-5-2)
	long := prefix + strings.Repeat("世", 4000)
	if len(long) < 10<<10 {
		t.Fatalf("fixture too short: %d", len(long))
	}
	reg := tools.NewRegistry(false, &fakeTool{name: "big", err: errors.New(long)})
	cs := connect(t, NewServer(reg, "v", nil))

	res := call(t, cs, "big", map[string]any{"name": "x"})
	if !res.IsError {
		t.Fatalf("want IsError, got %+v", res)
	}
	txt := textOf(t, res)
	if len(txt) > limit+len(marker)+1 {
		t.Fatalf("error text not bounded: %d bytes", len(txt))
	}
	if !strings.HasSuffix(txt, marker) {
		t.Fatalf("error text missing truncation marker: %q", txt[max(0, len(txt)-40):])
	}
	if !utf8.ValidString(txt) {
		t.Fatalf("error text is not valid UTF-8")
	}
	if !strings.HasPrefix(txt, "big:") {
		t.Fatalf("error text lost tool name prefix")
	}
}

func TestToolErrorSecretStraddlingCutIsRedacted(t *testing.T) {
	const (
		limit       = 2 << 10
		tokenSecret = "abc123.def-456_xyzQWERTY"
	)
	lead := "leaky: " + strings.Repeat("a", limit-len("leaky: ")-len("Bearer ")-8)
	long := lead + "Bearer " + tokenSecret + strings.Repeat("b", 10<<10)
	reg := tools.NewRegistry(false, &fakeTool{name: "leaky", err: errors.New(long)})
	cs := connect(t, NewServer(reg, "v", nil))

	res := call(t, cs, "leaky", map[string]any{"name": "x"})
	if !res.IsError {
		t.Fatalf("want IsError, got %+v", res)
	}
	txt := textOf(t, res)
	if len(txt) > limit+len("[truncated]")+1 {
		t.Fatalf("error text not bounded: %d bytes", len(txt))
	}
	if strings.Contains(txt, tokenSecret) || strings.Contains(txt, tokenSecret[:8]) {
		t.Fatalf("error text leaks secret (full or partial)")
	}
}

func TestUnknownToolRejected(t *testing.T) {
	reg := tools.NewRegistry(false, &fakeTool{name: "t", res: okResult("s")})
	cs := connect(t, NewServer(reg, "v", nil))
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "nope", Arguments: map[string]any{}})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("unknown tool must be rejected, got %+v", res)
	}
}

func TestNilLogger(t *testing.T) {
	// nil logger must not panic on construction or on calls.
	reg := tools.NewRegistry(false, &fakeTool{name: "t", res: okResult("s")}, &fakeTool{name: "e", err: errors.New("e: boom")})
	cs := connect(t, NewServer(reg, "v", nil))
	_ = call(t, cs, "t", map[string]any{"name": "x"})
	_ = call(t, cs, "e", map[string]any{"name": "x"})
}

func TestLogsMetadataOnly(t *testing.T) {
	var buf syncBuf
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := okResult("summary " + sumSecret)
	r.Truncated = true
	reg := tools.NewRegistry(false, &fakeTool{name: "t", res: r}, &fakeTool{name: "e", err: errors.New("e: boom")})
	cs := connect(t, NewServer(reg, "v", log))

	_ = call(t, cs, "t", map[string]any{"name": argSecret})
	_ = call(t, cs, "e", map[string]any{"name": argSecret})

	out := buf.String()
	for _, leak := range []string{argSecret, sumSecret, secretData} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaks %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{`"tool":"t"`, `"tool":"e"`, `"truncated":true`, `"token":2`, `"ok":false`, `"ok":true`, "duration"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	ft := &fakeTool{name: "slow", block: true}

	// Direct handler call with a cancelled context returns a Go error.
	h := newHandler(ft, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h(ctx, &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Name: "slow", Arguments: json.RawMessage(`{"name":"x"}`)}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	// Over the wire: client-side cancellation ends the call and the session survives.
	ok := &fakeTool{name: "ok", res: okResult("fine")}
	reg := tools.NewRegistry(false, ft, ok)
	cs := connect(t, NewServer(reg, "v", nil))
	cctx, ccancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer ccancel()
	if _, err := cs.CallTool(cctx, &sdk.CallToolParams{Name: "slow", Arguments: map[string]any{"name": "x"}}); err == nil {
		t.Fatal("want error from cancelled call")
	}
	if got := textOf(t, call(t, cs, "ok", map[string]any{"name": "x"})); got != "fine" {
		t.Fatalf("follow-up = %q", got)
	}
}

// TestRealToolsRegister builds the real six k8s tools against a fake clientset
// so every real schema goes through the SDK's AddTool validation (which panics
// on a schema that is not a JSON object of type "object").
func TestRealToolsRegister(t *testing.T) {
	realTools := k8stools.NewTools(kube.NewReader(fake.NewClientset(), config.NamespaceFilters{}))
	if len(realTools) != 6 {
		t.Fatalf("want 6 real tools, got %d", len(realTools))
	}
	for _, tl := range realTools {
		var m map[string]any
		if err := json.Unmarshal(tl.Schema(), &m); err != nil {
			t.Fatalf("%s: schema is not a JSON object: %v", tl.Name(), err)
		}
		if m["type"] != "object" {
			t.Fatalf("%s: schema type = %v, want object", tl.Name(), m["type"])
		}
	}

	cs := connect(t, NewServer(tools.NewRegistry(false, realTools...), "v", nil))
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		k8stools.NameGet, k8stools.NameListPods, k8stools.NameDescribe,
		k8stools.NameEvents, k8stools.NamePodLogs, k8stools.NameRolloutHistory,
	}
	sort.Strings(want)
	byName := map[string]tools.Tool{}
	for _, tl := range realTools {
		byName[tl.Name()] = tl
	}
	var got []string
	for _, tl := range lt.Tools {
		got = append(got, tl.Name)
		if tl.Description != byName[tl.Name].Description() {
			t.Errorf("%s: description mismatch", tl.Name)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	// Strict-decode rejections and missing arguments from a real tool
	// surface as IsError.
	for _, args := range []any{map[string]any{"bogus": true}, nil} {
		res := call(t, cs, k8stools.NameEvents, args)
		if !res.IsError || !strings.Contains(textOf(t, res), "invalid arguments") {
			t.Fatalf("args %v: want invalid-arguments IsError, got %+v", args, res)
		}
	}
}
