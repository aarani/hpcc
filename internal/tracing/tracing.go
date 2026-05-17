// Package tracing configures the project-wide OpenTelemetry tracer.
//
// Init wires an OTLP/gRPC exporter when the standard OTEL_EXPORTER_*
// env vars are set (see https://opentelemetry.io/docs/specs/otel/protocol/exporter/);
// otherwise tracing stays a no-op and instrumented call sites pay
// only the cost of a Tracer.Start/End pair on the noop tracer. This
// means binaries can be instrumented unconditionally without
// requiring an operator to run a collector for local development.
//
// Context propagation (W3C traceparent + baggage) is installed
// unconditionally so a traceparent header on an incoming RPC is
// honoured even when this process itself isn't exporting.
package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

// Init configures the global OpenTelemetry tracer for the process.
// Returns a shutdown function the caller should defer to flush
// pending spans on exit; the function is safe to call once and is a
// no-op when tracing is disabled.
//
// serviceName tags every emitted span with the OTel semantic
// `service.name` attribute so exporters can group traces by binary
// (e.g. "hpcc-worker", "hpcc-scheduler").
//
// Tracing is enabled only when OTEL_EXPORTER_OTLP_ENDPOINT (or the
// trace-specific OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is set. Without
// it we leave the global noop tracer in place and return a no-op
// shutdown — instrumentation in the rest of the codebase still
// compiles and runs but emits nothing.
func Init(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// Tracer returns a named tracer for an instrumentation package. Thin
// passthrough over otel.Tracer kept here so call sites import one
// place; the noop tracer is returned when Init hasn't run or has
// been disabled.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
