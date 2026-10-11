// Command agent-server runs the k8s-triage-agent HTTP server. In Phase 0 it
// loads configuration, initialises telemetry, and serves health endpoints.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/davidsugianto/upsera-agent/internal/config"
	"github.com/davidsugianto/upsera-agent/internal/telemetry"
)

const (
	defaultConfigPath = "configs/config.yaml"
	shutdownTimeout   = 10 * time.Second
	// telemetryFlushTimeout bounds the final telemetry export. It is separate
	// from shutdownTimeout because telemetry is best-effort: a collector that
	// is down must not delay or fail a normal shutdown.
	telemetryFlushTimeout = 3 * time.Second
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "agent-server:", err)
		os.Exit(1)
	}
}

// configPath picks the config file: the flag, then AGENT_CONFIG, then the default.
func configPath(flagValue, envValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if envValue != "" {
		return envValue
	}
	return defaultConfigPath
}

// newHandler builds the HTTP routes. /readyz reports 200 once ready is true.
func newHandler(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready\n")
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// run serves until ctx is cancelled, then shuts down gracefully. onListen, if
// non-nil, is called with the bound address once the listener is open.
func run(ctx context.Context, args []string, getenv func(string) string, logw io.Writer, onListen func(net.Addr)) error {
	fs := flag.NewFlagSet("agent-server", flag.ContinueOnError)
	fs.SetOutput(logw)
	cfgFlag := fs.String("config", "", "path to config file (env AGENT_CONFIG, default "+defaultConfigPath+")")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(logw, nil))
	path := configPath(*cfgFlag, getenv("AGENT_CONFIG"))
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	opts := telemetry.OptionsFromEnv(version, getenv)
	shutdownTelemetry, err := telemetry.Init(ctx, opts)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	if err := telemetry.RecordStartup(ctx); err != nil {
		return fmt.Errorf("record startup: %w", err)
	}

	var ready atomic.Bool
	srv := &http.Server{
		Handler:           newHandler(&ready),
		ReadTimeout:       cfg.Server.ReadTimeout,
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
	}
	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ready.Store(true)
	logger.Info("agent-server started", "addr", ln.Addr().String(), "config", path,
		"version", version, "clusters", len(cfg.Clusters), "otlp_enabled", opts.Endpoint != "")
	if onListen != nil {
		onListen(ln.Addr())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		flushTelemetry(logger, shutdownTelemetry)
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	ready.Store(false)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = srv.Shutdown(sctx)
	flushTelemetry(logger, shutdownTelemetry)
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// flushTelemetry exports what is buffered and stops the providers. A failure
// is logged and otherwise ignored: the agent's own observability being down is
// not a reason to fail a shutdown.
func flushTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Warn("telemetry flush failed; continuing shutdown", "error", err.Error())
	}
}
