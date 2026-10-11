package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	var ready atomic.Bool
	h := newHandler(&ready)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestReadyz(t *testing.T) {
	var ready atomic.Bool
	h := newHandler(&ready)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before ready: code=%d", rec.Code)
	}

	ready.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after ready: code=%d", rec.Code)
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	var ready atomic.Bool
	h := newHandler(&ready)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/nope code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz code=%d", rec.Code)
	}
}

func TestConfigPath(t *testing.T) {
	tests := []struct {
		name, flag, env, want string
	}{
		{"flag wins", "a.yaml", "b.yaml", "a.yaml"},
		{"env fallback", "", "b.yaml", "b.yaml"},
		{"default", "", "", defaultConfigPath},
	}
	for _, tt := range tests {
		if got := configPath(tt.flag, tt.env); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}

func TestRunBadConfig(t *testing.T) {
	err := run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "missing.yaml")},
		func(string) string { return "" }, io.Discard, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunServesAndShutsDownCleanly(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	body := `
server:
  addr: "127.0.0.1:0"
llm:
  provider: anthropic
  planner_model: p
  executor_model: e
datadog:
  site: datadoghq.com
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- run(ctx, []string{"--config", cfgPath}, func(string) string { return "" }, &logs,
			func(a net.Addr) { addrCh <- a })
	}()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get("http://" + addr.String() + path) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if !strings.Contains(logs.String(), `"msg"`) {
		t.Errorf("expected JSON logs, got %q", logs.String())
	}
}

// Telemetry is best-effort: when the collector is down, a normal shutdown must
// still finish quickly and cleanly instead of failing after the full timeout.
func TestRunShutsDownCleanlyWhenCollectorIsDown(t *testing.T) {
	// A port that was just open and is now closed: connections are refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadEndpoint := "http://" + ln.Addr().String()
	_ = ln.Close()
	// Injected through run's getenv, not the process environment.
	getenv := func(key string) string {
		if key == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			return deadEndpoint
		}
		return ""
	}

	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	body := `
server:
  addr: "127.0.0.1:0"
llm:
  provider: anthropic
  planner_model: p
  executor_model: e
datadog:
  site: datadoghq.com
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- run(ctx, []string{"--config", cfgPath}, getenv, &logs,
			func(a net.Addr) { addrCh <- a })
	}()
	select {
	case <-addrCh:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown with a dead collector returned %v, want nil", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown with a dead collector took more than 6s")
	}
	got := logs.String()
	if !strings.Contains(got, `"otlp_enabled":true`) {
		t.Errorf("the injected endpoint was not used (otlp_enabled should be true): %s", got)
	}
	if !strings.Contains(got, "telemetry flush failed") {
		t.Errorf("expected a warning that the telemetry flush failed: %s", got)
	}
	if !strings.Contains(got, `"msg":"stopped"`) {
		t.Errorf("expected a stopped log line, got %q", got)
	}
}
