package metrics

import (
	"context"
	"sync/atomic"

	"github.com/aarani/hpcc/internal/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// securityCounter mirrors every logging.Security() call as a
// counter sample labelled by (component, event, tenant_id). The
// hook is registered in init(); the underlying OTel instrument is
// created lazily on the first event so the global noop meter can
// be swapped in by Init() without an ordering constraint.
var (
	securityCounter   metric.Int64Counter
	securityComponent atomic.Value // string; set by SetComponent in main()
)

func init() {
	logging.SetSecurityHook(securityHook)
}

// SetComponent labels every metric emitted by this process with the
// binary's name ("scheduler", "worker", "agent", "daemon"). Should be
// called once during main(), before any RPC handlers run.
func SetComponent(name string) {
	securityComponent.Store(name)
}

func component() string {
	v, _ := securityComponent.Load().(string)
	return v
}

func ensureSecurityCounter() metric.Int64Counter {
	if securityCounter != nil {
		return securityCounter
	}
	c, err := Meter("github.com/aarani/hpcc/internal/metrics").
		Int64Counter("hpcc.security_events_total",
			metric.WithDescription("Misbehaving-client security events emitted via logging.Security"),
			metric.WithUnit("{event}"),
		)
	if err != nil {
		return nil
	}
	securityCounter = c
	return c
}

func securityHook(event string, fields []zap.Field) {
	c := ensureSecurityCounter()
	if c == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("component", component()),
		attribute.String("event", event),
	}
	if tid := tenantIDFromFields(fields); tid != "" {
		attrs = append(attrs, attribute.String("tenant_id", tid))
	}
	c.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

// tenantIDFromFields scans zap fields for the conventional
// "tenant_id" string field. Linear scan is fine — security-event
// field lists are short.
func tenantIDFromFields(fields []zap.Field) string {
	for _, f := range fields {
		if f.Key == "tenant_id" && f.Type == zapcore.StringType {
			return f.String
		}
	}
	return ""
}
