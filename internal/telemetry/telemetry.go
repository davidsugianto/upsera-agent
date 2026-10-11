// Package telemetry wires the agent's own OpenTelemetry traces and metrics.
//
// OTLP gRPC exporters are created only when an endpoint is configured;
// otherwise the global providers are no-ops, so the agent runs unchanged
// without a collector.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	// DefaultServiceName is used when no service name is configured.
	DefaultServiceName = "k8s-triage-agent"

	instrumentationName = "github.com/davidsugianto/upsera-agent"
	startupSpanName     = "agent.startup"
	upMetricName        = "k8s_triage_agent.up"
)

// Options configures Init.
type Options struct {
	// ServiceName defaults to DefaultServiceName.
	ServiceName    string
	ServiceVersion string
	// Endpoint is the OTLP gRPC endpoint (for example http://localhost:4317).
	// Empty means telemetry is a no-op, unless exporters are injected.
	Endpoint string

	// SpanExporter and MetricReader override the OTLP exporters. They exist so
	// tests can capture telemetry in memory.
	SpanExporter sdktrace.SpanExporter
	MetricReader sdkmetric.Reader
}

// OptionsFromEnv builds Options from the standard OTEL_* environment variables,
// read through getenv (os.Getenv in production) so callers and tests can inject
// them without touching the process environment.
func OptionsFromEnv(version string, getenv func(string) string) Options {
	name := getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = DefaultServiceName
	}
	return Options{
		ServiceName:    name,
		ServiceVersion: version,
		Endpoint:       getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
}

// Init installs the global tracer and meter providers and returns a function
// that flushes and stops them.
func Init(ctx context.Context, opts Options) (func(context.Context) error, error) {
	if opts.ServiceName == "" {
		opts.ServiceName = DefaultServiceName
	}

	var shutdowns []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			errs = append(errs, shutdowns[i](ctx))
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (func(context.Context) error, error) {
		return nil, errors.Join(err, shutdown(ctx))
	}

	spanExp := opts.SpanExporter
	syncSpans := spanExp != nil // injected exporters export synchronously
	reader := opts.MetricReader
	if opts.Endpoint != "" {
		if spanExp == nil {
			exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(opts.Endpoint))
			if err != nil {
				return fail(fmt.Errorf("create OTLP trace exporter: %w", err))
			}
			spanExp = exp
		}
		if reader == nil {
			exp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(opts.Endpoint))
			if err != nil {
				return fail(fmt.Errorf("create OTLP metric exporter: %w", err))
			}
			reader = sdkmetric.NewPeriodicReader(exp)
		}
	}

	if spanExp == nil && reader == nil {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		return shutdown, nil
	}

	attrs := []attribute.KeyValue{attribute.String("service.name", opts.ServiceName)}
	if opts.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", opts.ServiceVersion))
	}
	res := resource.NewSchemaless(attrs...)

	if spanExp != nil {
		tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
		if syncSpans {
			tpOpts = append(tpOpts, sdktrace.WithSyncer(spanExp))
		} else {
			tpOpts = append(tpOpts, sdktrace.WithBatcher(spanExp))
		}
		tp := sdktrace.NewTracerProvider(tpOpts...)
		shutdowns = append(shutdowns, tp.Shutdown)
		otel.SetTracerProvider(tp)
	} else {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
	}
	if reader != nil {
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
		shutdowns = append(shutdowns, mp.Shutdown)
		otel.SetMeterProvider(mp)
	} else {
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
	}
	return shutdown, nil
}

// RecordStartup emits the agent.startup span and sets the
// k8s_triage_agent.up gauge to 1.
func RecordStartup(ctx context.Context) error {
	_, span := otel.GetTracerProvider().Tracer(instrumentationName).Start(ctx, startupSpanName,
		trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	up, err := otel.GetMeterProvider().Meter(instrumentationName).Int64Gauge(upMetricName,
		metric.WithDescription("1 while the agent process is running"))
	if err != nil {
		return fmt.Errorf("create %s gauge: %w", upMetricName, err)
	}
	up.Record(ctx, 1)
	return nil
}
