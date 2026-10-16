package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const stdioTestConfig = `server:
  addr: ":8080"
  read_timeout: 15s
agent:
  max_tool_calls: 25
  max_llm_tokens: 30000
  max_cost_usd: 0.25
  max_duration: 120s
llm:
  provider: anthropic
  planner_model: test-planner
  executor_model: test-executor
  api_key_env: ANTHROPIC_API_KEY
datadog:
  site: datadoghq.com
  api_key_env: DD_API_KEY
  app_key_env: DD_APP_KEY
clusters:
  - name: test-cluster
    kubeconfig_context: test-ctx
    namespaces:
      allow: []
      deny: [kube-system]
`

// An unreachable server and a dummy token: building the client must not need
// the network, and tools/list must never contact the cluster.
const stdioTestKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: test-cluster
    cluster:
      server: https://127.0.0.1:1
users:
  - name: test-user
    user:
      token: dummy-token
contexts:
  - name: test-ctx
    context:
      cluster: test-cluster
      user: test-user
current-context: test-ctx
`

// The binary is built once per set of concurrently running tests and shared; a
// refcount removes it when the last user's cleanup runs. Tests run one after
// another, so each test (and each -count=N iteration) rebuilds it.
var (
	stdioMu     sync.Mutex
	stdioUsers  int
	stdioBinary string
	stdioDir    string
)

// moduleRoot walks up from this source file to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above " + file)
		}
		dir = parent
	}
}

// buildAgentCLI builds the binary once and returns its path.
func buildAgentCLI(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("skipping binary-level stdio test: `go` is not on PATH")
	}
	root := moduleRoot(t)
	stdioMu.Lock()
	defer stdioMu.Unlock()
	if stdioBinary == "" {
		dir, err := os.MkdirTemp("", "agent-cli-stdio-")
		if err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(dir, "agent-cli")
		cmd := exec.Command(goBin, "build", "-o", bin, "./cmd/agent-cli") //nolint:gosec // fixed args
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			t.Fatalf("go build failed: %v\n%s", err, out)
		}
		stdioDir, stdioBinary = dir, bin
	}
	stdioUsers++
	t.Cleanup(func() {
		stdioMu.Lock()
		defer stdioMu.Unlock()
		stdioUsers--
		if stdioUsers == 0 {
			_ = os.RemoveAll(stdioDir)
			stdioDir, stdioBinary = "", ""
		}
	})
	return stdioBinary
}

// lockedBuffer is a bytes.Buffer safe to read while exec's copying goroutine
// is still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeStdioFixtures(t *testing.T) (cfg, kc string) {
	t.Helper()
	dir := t.TempDir()
	cfg = filepath.Join(dir, "config.yaml")
	kc = filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(cfg, []byte(stdioTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kc, []byte(stdioTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, kc
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func TestMCPStdioBinary(t *testing.T) {
	bin := buildAgentCLI(t)
	cfg, kc := writeStdioFixtures(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp", "--config", cfg, "--kubeconfig", kc) //nolint:gosec // test binary
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	})

	// Collect stdout lines on a goroutine so the test can wait with a deadline.
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	send := func(s string) {
		t.Helper()
		if _, err := stdin.Write([]byte(s + "\n")); err != nil {
			t.Fatalf("write to stdin: %v\nstderr:\n%s", err, stderr.String())
		}
	}
	// next returns the next non-empty stdout line, validating it as JSON-RPC.
	next := func() rpcMsg {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stdout closed early\nstderr:\n%s", stderr.String())
				}
				if l == "" {
					continue
				}
				var m rpcMsg
				if err := json.Unmarshal([]byte(l), &m); err != nil {
					t.Fatalf("stdout line is not JSON: %q: %v", l, err)
				}
				if m.JSONRPC != "2.0" {
					t.Fatalf("stdout line is not JSON-RPC 2.0: %q", l)
				}
				return m
			case <-time.After(15 * time.Second):
				t.Fatalf("timed out waiting for a response\nstderr:\n%s", stderr.String())
			}
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stdio-test","version":"0"}}}`)
	if m := next(); string(m.ID) != "1" || m.Error != nil || m.Result == nil {
		t.Fatalf("bad initialize response: %+v", m)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	m := next()
	if string(m.ID) != "2" || m.Error != nil {
		t.Fatalf("bad tools/list response: %+v", m)
	}
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(m.Result, &res); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}
	slices.Sort(got)
	want := []string{"k8s_describe", "k8s_events", "k8s_get", "k8s_list_pods", "pod_logs", "rollout_history"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	// Closing stdin is a normal shutdown: exit 0, and nothing else on stdout.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
drain:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				break drain
			}
			if l == "" {
				continue
			}
			var x rpcMsg
			if err := json.Unmarshal([]byte(l), &x); err != nil || x.JSONRPC != "2.0" {
				t.Fatalf("non-protocol stdout line after tools/list: %q", l)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stdout did not close within 5s of stdin closing\nstderr:\n%s", stderr.String())
		}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Fatalf("exit after stdin close: %v\nstderr:\n%s", err, stderr.String())
		}
		abs, err := filepath.Abs(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`msg="mcp config"`, "config=" + abs, "cluster=test-cluster", "context=test-ctx"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr.String())
			}
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		waited = true
		t.Fatalf("process did not exit within 5s of stdin closing\nstderr:\n%s", stderr.String())
	}
}

func TestMCPStdioBinaryMissingConfig(t *testing.T) {
	bin := buildAgentCLI(t)
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp", "--config", missing) //nolint:gosec // test binary
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("err = %v, want exit code 1\nstderr:\n%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must be empty, got %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("expected an error message on stderr")
	}
}
