package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidsugianto/upsera-agent/internal/config"
	mcphandler "github.com/davidsugianto/upsera-agent/internal/handler/mcp"
)

// runMCPServer serves srv until the client disconnects or ctx ends. A seam so
// tests can use an in-memory transport instead of the process's stdio.
var runMCPServer = func(ctx context.Context, srv *sdk.Server) error {
	return srv.Run(ctx, &sdk.StdioTransport{})
}

// runMCP implements `agent-cli mcp` and returns the exit code. stdout is the
// protocol channel, so this command never writes to it; everything else goes
// to stderr.
func runMCP(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-cli mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cluster := fs.String("cluster", "", "cluster name from the config (required unless the config has exactly one)")
	configPath := fs.String("config", "", "path to the config file (required; use an absolute path: MCP clients start the server in the project directory)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: KUBECONFIG or ~/.kube/config)")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: agent-cli mcp [flags]\n\n"+
			"Serve the read-only Kubernetes tools to an MCP client over stdio.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agent-cli: "+format+"\n", a...)
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		return usageErr("mcp takes no arguments")
	}

	if *configPath == "" {
		return usageErr("--config is required (use an absolute path: MCP clients start the server in the project directory)")
	}
	// A relative path resolves against the cwd, which an MCP client sets to the
	// open project, so a repository could supply the config that picks the cluster.
	if !filepath.IsAbs(*configPath) {
		return usageErr("--config must be an absolute path, got %q: MCP clients start the server in the project directory", *configPath)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}
	cl, isUsage, err := selectCluster(cfg, *cluster)
	if err != nil {
		if isUsage {
			return usageErr("%v", err)
		}
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}

	reg, err := newRegistry(cl, *kubeconfig)
	if err != nil {
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		absConfig = *configPath
	}
	logger.Info("mcp config", "config", absConfig, "cluster", cl.Name, "context", cl.KubeconfigContext)
	srv := mcphandler.NewServer(reg, version, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runMCPServer(ctx, srv); err != nil {
		// A signal or the client closing stdin is a normal way to stop.
		if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return 0
		}
		fmt.Fprintf(stderr, "agent-cli: mcp: %v\n", err)
		return 1
	}
	return 0
}
