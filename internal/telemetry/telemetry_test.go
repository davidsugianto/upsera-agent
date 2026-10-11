package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func attrValue(t *testing.T, res *resource.Resource, key string) string {
	t.Helper()
	v, ok := res.Set().Value(attribute.Key(key))
	if !ok {
		t.Fatalf("resource missing %s", key)
	}
	return v.AsString()
}

func TestInitNoEndpointIsNoop(t *testing.T) {
	shutdown, err := Init(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Must not panic or error with no exporters configured.
	if err := RecordStartup(context.Background()); err != nil {
		t.Fatalf("RecordStartup: %v", err)
	}
	_, span := otel.Tracer("t").Start(context.Background(), "x")
	if span.SpanContext().IsValid() {
		t.Error("expected no-op span with no endpoint")
	}
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitWithInjectedExporters(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	ctx := context.Background()

	shutdown, err := Init(ctx, Options{
		ServiceVersion: "1.2.3",
		SpanExporter:   spans,
		MetricReader:   reader,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := RecordStartup(ctx); err != nil {
		t.Fatalf("RecordStartup: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := attrValue(t, rm.Resource, "service.name"); got != DefaultServiceName {
		t.Errorf("metric service.name = %q", got)
	}
	if got := attrValue(t, rm.Resource, "service.version"); got != "1.2.3" {
		t.Errorf("metric service.version = %q", got)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "k8s_triage_agent.up" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || len(g.DataPoints) != 1 || g.DataPoints[0].Value != 1 {
				t.Fatalf("unexpected gauge data: %#v", m.Data)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("metric k8s_triage_agent.up not recorded")
	}

	got := spans.GetSpans()
	if len(got) != 1 || got[0].Name != "agent.startup" {
		t.Fatalf("spans = %v", got)
	}
	if v := attrValue(t, got[0].Resource, "service.name"); v != DefaultServiceName {
		t.Errorf("span service.name = %q", v)
	}
	if v := attrValue(t, got[0].Resource, "service.version"); v != "1.2.3" {
		t.Errorf("span service.version = %q", v)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitCustomServiceName(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	shutdown, err := Init(context.Background(), Options{ServiceName: "custom", SpanExporter: spans})
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := spans.GetSpans()
	if len(got) != 1 || attrValue(t, got[0].Resource, "service.name") != "custom" {
		t.Fatalf("spans = %v", got)
	}
	_ = shutdown(context.Background())
}

func TestOptionsFromEnv(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317",
		"OTEL_SERVICE_NAME":           "svc",
	}
	o := OptionsFromEnv("v9", func(k string) string { return env[k] })
	if o.Endpoint != "http://localhost:4317" || o.ServiceName != "svc" || o.ServiceVersion != "v9" {
		t.Errorf("opts = %+v", o)
	}
	o = OptionsFromEnv("v9", func(string) string { return "" })
	if o.Endpoint != "" || o.ServiceName != DefaultServiceName {
		t.Errorf("opts = %+v", o)
	}
}

func TestInitWithEndpointBuildsExporters(t *testing.T) {
	// gRPC exporters connect lazily, so an unreachable endpoint still inits.
	shutdown, err := Init(context.Background(), Options{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(sctx)
}
