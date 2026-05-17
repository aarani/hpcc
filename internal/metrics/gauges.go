package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Observable gauges expose snapshot state — "how many of X are there
// right now" — that doesn't fit a counter or histogram. Callers
// register an observer callback once at startup; the OTel SDK
// invokes it on every collection cycle (Prometheus scrape or OTLP
// push tick).
//
// All gauges below are wired into the same meter so they share
// resource attributes (service.name, host, process) with the
// counters in instruments.go.

// RegisterDaemonInflight registers a callback that reports the
// daemon's current in-flight compile count. Call once during
// daemon startup, after metrics.Init. The returned registration is
// kept alive by the global meter; ignore it unless you specifically
// want to revoke (rare in production paths).
func RegisterDaemonInflight(inflight func() int32) error {
	gauge, err := Meter("github.com/aarani/hpcc").Int64ObservableGauge(
		"hpcc.daemon.inflight_compiles",
		metric.WithDescription("Compile requests currently in flight on the daemon"),
		metric.WithUnit("{compile}"),
	)
	if err != nil {
		return err
	}
	_, err = Meter("github.com/aarani/hpcc").RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(gauge, int64(inflight()))
			return nil
		},
		gauge,
	)
	return err
}

// RegisterWorkerInflight registers a callback that reports the
// worker's current in-flight Compile RPC count.
func RegisterWorkerInflight(inflight func() int32) error {
	gauge, err := Meter("github.com/aarani/hpcc").Int64ObservableGauge(
		"hpcc.worker.inflight_compiles",
		metric.WithDescription("Compile RPCs currently in flight on this worker"),
		metric.WithUnit("{compile}"),
	)
	if err != nil {
		return err
	}
	_, err = Meter("github.com/aarani/hpcc").RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(gauge, int64(inflight()))
			return nil
		},
		gauge,
	)
	return err
}

// RegisterWorkerContainers registers a callback that reports the
// number of live container pool entries broken down by tenant_id.
// The callback returns a snapshot map; the SDK emits one observation
// per (tenant_id) label set per collection. tenant_id is bounded
// cardinality in any realistic deployment (= number of tenants
// configured on the scheduler).
func RegisterWorkerContainers(byTenant func() map[string]int) error {
	gauge, err := Meter("github.com/aarani/hpcc").Int64ObservableGauge(
		"hpcc.worker.active_containers",
		metric.WithDescription("Live container pool entries on this worker, by tenant"),
		metric.WithUnit("{container}"),
	)
	if err != nil {
		return err
	}
	_, err = Meter("github.com/aarani/hpcc").RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			for tenant, n := range byTenant() {
				o.ObserveInt64(gauge, int64(n),
					metric.WithAttributes(attribute.String("tenant_id", tenant)),
				)
			}
			return nil
		},
		gauge,
	)
	return err
}

// RegisterSchedulerWorkers registers a callback that reports the
// scheduler's current count of registered workers.
func RegisterSchedulerWorkers(count func() int) error {
	gauge, err := Meter("github.com/aarani/hpcc").Int64ObservableGauge(
		"hpcc.scheduler.registered_workers",
		metric.WithDescription("Workers currently registered with the scheduler"),
		metric.WithUnit("{worker}"),
	)
	if err != nil {
		return err
	}
	_, err = Meter("github.com/aarani/hpcc").RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(gauge, int64(count()))
			return nil
		},
		gauge,
	)
	return err
}
