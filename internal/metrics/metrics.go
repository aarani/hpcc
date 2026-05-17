// Package metrics configures the project-wide OpenTelemetry metrics
// pipeline.
//
// Two readers are wired and either, both, or neither can be active:
//
//   - **Prometheus scrape** (server-side: scheduler, worker). Enabled
//     by passing PrometheusListen != "" to Init; the returned
//     http.Handler is mounted by the caller on a dedicated listener.
//     Always-on for servers because operators expect to be able to
//     scrape; the cost is one in-process registry and a goroutine.
//
//   - **OTLP/gRPC push** (any binary, gated on the standard
//     OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
//     env vars). The daemon runs on client machines that usually
//     can't be scraped, so push is the only realistic surface there
//     — and gating on env vars keeps the default a no-op so dev
//     laptops never need a collector.
//
// Both readers share one MeterProvider, so instruments declared once
// at startup feed whichever exporter is active. Resource attributes
// carry service.name (passed in by the caller) plus
// OTEL_RESOURCE_ATTRIBUTES, host, and process metadata via
// resource.WithFromEnv/WithHost/WithProcess.
//
// Instruments live in sibling subpackages
// (internal/metrics/{daemon,scheduler,worker}/...) — this package
// only owns the SDK plumbing.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	metricapi "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// Options controls which readers are wired into the MeterProvider.
type Options struct {
	// ServiceName tags every metric stream with the OTel semantic
	// `service.name` attribute. Required.
	ServiceName string

	// PrometheusReader is true on binaries that should expose a
	// /metrics scrape endpoint (scheduler, worker). The returned
	// *Result.PromHandler is non-nil when true.
	PrometheusReader bool
}

// Result is what Init hands back. PromHandler is the http.Handler the
// caller should mount on its scrape listener (nil when
// Options.PrometheusReader is false). Shutdown flushes the periodic
// OTLP reader and is safe to call once.
type Result struct {
	PromHandler http.Handler
	Shutdown    func(context.Context) error
}

// Init configures the global MeterProvider and returns the bits the
// caller has to wire up itself (the Prometheus HTTP handler). On
// error the global provider is left untouched and Shutdown is a
// no-op.
func Init(ctx context.Context, opts Options) (*Result, error) {
	if opts.ServiceName == "" {
		return nil, errors.New("metrics.Init: ServiceName is required")
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(opts.ServiceName)),
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	mpOpts := []metric.Option{metric.WithResource(res)}

	var promHandler http.Handler
	if opts.PrometheusReader {
		// Use a dedicated registry so hpcc's metrics don't collide
		// with anything else mounted on the same process (and so a
		// test process can wire its own without picking up the
		// default registry's Go-runtime collectors twice).
		reg := prometheus.NewRegistry()
		exp, err := otelprom.New(otelprom.WithRegisterer(reg))
		if err != nil {
			return nil, fmt.Errorf("build prometheus exporter: %w", err)
		}
		mpOpts = append(mpOpts, metric.WithReader(exp))
		promHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			Registry: reg,
		})
	}

	otlpEnabled := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
	if otlpEnabled {
		exp, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("build OTLP metric exporter: %w", err)
		}
		mpOpts = append(mpOpts, metric.WithReader(metric.NewPeriodicReader(exp)))
	}

	mp := metric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(mp)

	return &Result{
		PromHandler: promHandler,
		Shutdown: func(shutdownCtx context.Context) error {
			return mp.Shutdown(shutdownCtx)
		},
	}, nil
}

// Meter returns a named OTel meter. Thin passthrough so call sites
// import one place; the noop meter is returned when Init hasn't run.
func Meter(name string) metricapi.Meter {
	return otel.Meter(name)
}

// ServePrometheus runs a tiny http.Server that exposes /metrics on
// addr. Blocks until ctx is cancelled (then triggers a graceful
// shutdown with a 5s deadline) or the listener fails.
//
// Callers that already have an HTTP mux of their own can ignore this
// helper and mount Result.PromHandler themselves.
func ServePrometheus(ctx context.Context, addr string, handler http.Handler) error {
	if handler == nil {
		return errors.New("metrics.ServePrometheus: handler is nil")
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}
