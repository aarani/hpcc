package metrics

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Result values for the compile counters. Stable; emitted as the
// `result` attribute on `hpcc.{daemon,worker}.compiles_total` so
// dashboards can build a rate-by-outcome view.
const (
	ResultLocalHit         = "local_hit"         // daemon: served from on-disk cache
	ResultRemote           = "remote"            // daemon: served by a worker (any source mode)
	ResultLocalInvoke      = "local_invoke"      // daemon: ran the compiler in-process (fallback or no dispatcher)
	ResultBypass           = "bypass"            // daemon: argv unrepresentable, ran locally without caching
	ResultError            = "error"             // any binary: compile/dispatch failed
	ResultOK               = "ok"                // any binary: succeeded
	ResultCacheHit         = "cache_hit"         // worker: probe-then-serve from worker-side cache
	ResultTenantUnknown    = "tenant_unknown"    // scheduler: Authenticate with an unconfigured tenant
	ResultAuthFailed       = "auth_failed"       // scheduler: IdP rejected or JWT invalid
	ResultNoWorker         = "no_worker"         // scheduler: Route had nobody to hand the client to
	ResultWorkerUnreachable = "worker_unreachable" // daemon: dispatcher failed before/after the worker call
)

// Direction values for CAS byte/blob counters.
const (
	DirectionUpload   = "upload"
	DirectionDownload = "download"
)

// instruments lazily backs every counter/histogram below. Resolved on
// first use against whatever the global MeterProvider is at that
// point — Init swaps in a real provider; tests and the
// no-Init-yet window get the noop meter and silently drop samples.
type instruments struct {
	once sync.Once

	daemonCompiles    metric.Int64Counter
	daemonDuration    metric.Float64Histogram
	workerCompiles    metric.Int64Counter
	workerDuration    metric.Float64Histogram
	workerCASBytes    metric.Int64Counter
	workerCASBlobs    metric.Int64Counter
	schedulerAuth     metric.Int64Counter
	schedulerRoutes   metric.Int64Counter
	schedulerHeartbts metric.Int64Counter
}

var inst = &instruments{}

func (i *instruments) init() {
	m := Meter("github.com/aarani/hpcc")
	i.daemonCompiles, _ = m.Int64Counter("hpcc.daemon.compiles_total",
		metric.WithDescription("Compile invocations served by the daemon, by outcome"),
		metric.WithUnit("{compile}"),
	)
	i.daemonDuration, _ = m.Float64Histogram("hpcc.daemon.compile_duration_seconds",
		metric.WithDescription("End-to-end duration of one daemon-handled compile"),
		metric.WithUnit("s"),
	)
	i.workerCompiles, _ = m.Int64Counter("hpcc.worker.compiles_total",
		metric.WithDescription("Compile RPCs served by this worker, by tenant and outcome"),
		metric.WithUnit("{compile}"),
	)
	i.workerDuration, _ = m.Float64Histogram("hpcc.worker.compile_duration_seconds",
		metric.WithDescription("End-to-end duration of one worker-served Compile RPC"),
		metric.WithUnit("s"),
	)
	i.workerCASBytes, _ = m.Int64Counter("hpcc.worker.cas_bytes_total",
		metric.WithDescription("Bytes transferred through CAS RPCs on this worker, by direction"),
		metric.WithUnit("By"),
	)
	i.workerCASBlobs, _ = m.Int64Counter("hpcc.worker.cas_blobs_total",
		metric.WithDescription("Blob count transferred through CAS RPCs on this worker, by direction"),
		metric.WithUnit("{blob}"),
	)
	i.schedulerAuth, _ = m.Int64Counter("hpcc.scheduler.auth_total",
		metric.WithDescription("Authenticate RPCs, by outcome"),
		metric.WithUnit("{request}"),
	)
	i.schedulerRoutes, _ = m.Int64Counter("hpcc.scheduler.routes_total",
		metric.WithDescription("Route RPCs, by tenant and outcome"),
		metric.WithUnit("{request}"),
	)
	i.schedulerHeartbts, _ = m.Int64Counter("hpcc.scheduler.heartbeats_total",
		metric.WithDescription("Worker heartbeat RPCs, by outcome"),
		metric.WithUnit("{heartbeat}"),
	)
}

func get() *instruments {
	inst.once.Do(inst.init)
	return inst
}

// DaemonCompile records one daemon-served compile and its duration.
// result is one of the Result* constants above.
func DaemonCompile(ctx context.Context, result string, duration time.Duration) {
	i := get()
	attrs := metric.WithAttributes(attribute.String("result", result))
	if i.daemonCompiles != nil {
		i.daemonCompiles.Add(ctx, 1, attrs)
	}
	if i.daemonDuration != nil {
		i.daemonDuration.Record(ctx, duration.Seconds(), attrs)
	}
}

// WorkerCompile records one worker-served Compile RPC. tenantID is
// the verified tenant from the descriptor (empty when the request was
// rejected before the descriptor was trusted — those go through
// IncSecurityEvent instead).
func WorkerCompile(ctx context.Context, tenantID, result string, duration time.Duration) {
	i := get()
	attrs := metric.WithAttributes(
		attribute.String("tenant_id", tenantID),
		attribute.String("result", result),
	)
	if i.workerCompiles != nil {
		i.workerCompiles.Add(ctx, 1, attrs)
	}
	if i.workerDuration != nil {
		i.workerDuration.Record(ctx, duration.Seconds(), attrs)
	}
}

// WorkerCASTransfer records one upload/download against the CAS on
// this worker. blobs is the count this call moved; bytes is their
// summed size.
func WorkerCASTransfer(ctx context.Context, direction string, blobs, bytes int64) {
	i := get()
	attrs := metric.WithAttributes(attribute.String("direction", direction))
	if i.workerCASBlobs != nil && blobs > 0 {
		i.workerCASBlobs.Add(ctx, blobs, attrs)
	}
	if i.workerCASBytes != nil && bytes > 0 {
		i.workerCASBytes.Add(ctx, bytes, attrs)
	}
}

// SchedulerAuth records one Authenticate RPC.
func SchedulerAuth(ctx context.Context, result string) {
	i := get()
	if i.schedulerAuth == nil {
		return
	}
	i.schedulerAuth.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// SchedulerRoute records one Route RPC.
func SchedulerRoute(ctx context.Context, tenantID, result string) {
	i := get()
	if i.schedulerRoutes == nil {
		return
	}
	i.schedulerRoutes.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant_id", tenantID),
		attribute.String("result", result),
	))
}

// SchedulerHeartbeat records one worker heartbeat RPC.
func SchedulerHeartbeat(ctx context.Context, result string) {
	i := get()
	if i.schedulerHeartbts == nil {
		return
	}
	i.schedulerHeartbts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
