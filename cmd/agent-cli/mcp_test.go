package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	"github.com/davidsugianto/upsera-agent/internal/config"
)

// stubMCP replaces the registry builder and the run step for one test.
func stubMCP(t *testing.T, regErr error, run func(ctx context.Context, srv *sdk.Server) error) *[]factoryCall {
	t.Helper()
	var calls []factoryCall
	origReg, origRun := newRegistry, runMCPServer
	newRegistry = func(cl config.ClusterConfig, kubeconfig string) (*tools.Registry, error) {
		calls = append(calls, factoryCall{cluster: cl, kubeconfig: kubeconfig})
		if regErr != nil {
			return nil, regErr
		}
		return tools.NewRegistry(false), nil
	}
	if run != nil {
		runMCPServer = run
	}
	t.Cleanup(func() { newRegistry, runMCPServer = origReg, origRun })
	return &calls
}

func TestMCPUsageErrorsExit2(t *testing.T) {
	cfg := writeConfig(t, oneCluster)
	cases := map[string][]string{
		"positional": {"--config", cfg, "extra"},
		"bad flag":   {"--nope"},
		"ambiguous":  {"--config", writeConfig(t, twoClusters)},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			calls := stubMCP(t, nil, func(context.Context, *sdk.Server) error {
				t.Fatal("server must not run on a usage error")
				return nil
			})
			var out, errOut bytes.Buffer
			if code := runMCP(args, &out, &errOut); code != 2 {
				t.Fatalf("code=%d stderr=%q", code, errOut.String())
			}
			if out.Len() != 0 {
				t.Errorf("stdout=%q, want empty", out.String())
			}
			if errOut.Len() == 0 {
				t.Error("want a message on stderr")
			}
			if len(*calls) != 0 {
				t.Errorf("registry built on usage error")
			}
		})
	}
}

func TestMCPConfigRequired(t *testing.T) {
	calls := stubMCP(t, nil, func(context.Context, *sdk.Server) error {
		t.Fatal("server must not run without --config")
		return nil
	})
	// A config in the cwd must never be picked up implicitly.
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("configs", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("configs", "config.yaml"), []byte(configHead+oneCluster), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runMCP(nil, &out, &errOut); code != 2 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout=%q, want empty", out.String())
	}
	for _, want := range []string{"--config is required", "absolute path", "Usage: agent-cli mcp"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q: %q", want, errOut.String())
		}
	}
	if len(*calls) != 0 {
		t.Error("registry built without --config")
	}
}

// A relative --config resolves against the cwd, which an MCP client sets to the
// open project, so it is refused just like a missing one.
func TestMCPConfigMustBeAbsolute(t *testing.T) {
	calls := stubMCP(t, nil, func(context.Context, *sdk.Server) error {
		t.Fatal("server must not run with a relative --config")
		return nil
	})
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("configs", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("configs", "config.yaml"), []byte(configHead+oneCluster), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runMCP([]string{"--config", filepath.Join("configs", "config.yaml")}, &out, &errOut); code != 2 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout=%q, want empty", out.String())
	}
	for _, want := range []string{"--config must be an absolute path", "Usage: agent-cli mcp"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q: %q", want, errOut.String())
		}
	}
	if len(*calls) != 0 {
		t.Error("registry built with a relative --config")
	}
}

func TestMCPLogsConfigOnStderr(t *testing.T) {
	stubMCP(t, nil, func(context.Context, *sdk.Server) error { return nil })
	cfg := writeConfig(t, oneCluster)
	var out, errOut bytes.Buffer
	if code := runMCP([]string{"--config", cfg}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout=%q, want empty", out.String())
	}
	abs, err := filepath.Abs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`msg="mcp config"`, "config=" + abs, "cluster=lab", "context=kind-lab"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q: %q", want, errOut.String())
		}
	}
	if strings.Contains(errOut.String(), "kube-system") {
		t.Errorf("stderr must not log namespace lists: %q", errOut.String())
	}
}

func TestMCPHelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runMCP([]string{"--help"}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout=%q, want empty", out.String())
	}
	for _, f := range []string{"--config", "--cluster", "--kubeconfig"} {
		if !strings.Contains(errOut.String(), strings.TrimPrefix(f, "-")) {
			t.Errorf("help missing %s: %q", f, errOut.String())
		}
	}
	if !strings.Contains(errOut.String(), "config file (required") {
		t.Errorf("help must say --config is required: %q", errOut.String())
	}
}

func TestMCPConfigAndClusterErrorsExit1(t *testing.T) {
	cases := map[string][]string{
		"missing config":  {"--config", filepath.Join(t.TempDir(), "nope.yaml")},
		"unknown cluster": {"--config", writeConfig(t, oneCluster), "--cluster", "ghost"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			calls := stubMCP(t, nil, nil)
			var out, errOut bytes.Buffer
			if code := runMCP(args, &out, &errOut); code != 1 {
				t.Fatalf("code=%d stderr=%q", code, errOut.String())
			}
			if out.Len() != 0 {
				t.Errorf("stdout=%q, want empty", out.String())
			}
			if !strings.Contains(errOut.String(), "agent-cli:") {
				t.Errorf("stderr=%q", errOut.String())
			}
			if len(*calls) != 0 {
				t.Errorf("registry built on config error")
			}
		})
	}
}

func TestMCPRegistryErrorExits1(t *testing.T) {
	calls := stubMCP(t, errors.New("cluster unreachable"), func(context.Context, *sdk.Server) error {
		t.Fatal("server must not run without a registry")
		return nil
	})
	var out, errOut bytes.Buffer
	code := runMCP([]string{"--config", writeConfig(t, oneCluster), "--kubeconfig", "/kc"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout=%q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "cluster unreachable") {
		t.Errorf("stderr=%q", errOut.String())
	}
	if len(*calls) != 1 || (*calls)[0].cluster.Name != "lab" || (*calls)[0].kubeconfig != "/kc" {
		t.Errorf("calls=%+v", *calls)
	}
}

func TestMCPRunErrorExits1(t *testing.T) {
	stubMCP(t, nil, func(context.Context, *sdk.Server) error { return errors.New("transport broke") })
	var out, errOut bytes.Buffer
	if code := runMCP([]string{"--config", writeConfig(t, oneCluster)}, &out, &errOut); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "transport broke") {
		t.Errorf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestMCPRoundTripAndCleanShutdown(t *testing.T) {
	serverT, clientT := sdk.NewInMemoryTransports()
	stubMCP(t, nil, func(ctx context.Context, srv *sdk.Server) error { return srv.Run(ctx, serverT) })

	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runMCP([]string{"--config", writeConfig(t, oneCluster)}, &out, &errOut)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil) // performs initialize
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Logf("client close: %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d stderr=%q", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runMCP did not return after the client disconnected")
	}
	if out.Len() != 0 {
		t.Errorf("stdout=%q, want empty", out.String())
	}
}

func TestMainDispatchesMCP(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"mcp", "extra"}, &out, &errOut); code != 2 {
		t.Fatalf("code=%d", code)
	}
}
