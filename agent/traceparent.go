package main

import "strings"

// traceIDFromTraceparent extracts the 32-hex trace_id segment from a
// W3C traceparent header (`<version>-<trace_id>-<span_id>-<flags>`).
// Returns "" for any malformed input — non-strict by design, this is
// for observability not security.
//
// Kept as a hand-written parser so the agent module doesn't depend on
// the OpenTelemetry SDK just to surface the trace ID in its logs.
// When/if in-VM tracing lands, replace with propagation.TraceContext
// from go.opentelemetry.io/otel.
func traceIDFromTraceparent(tp string) string {
	if tp == "" {
		return ""
	}
	parts := strings.Split(tp, "-")
	if len(parts) < 4 {
		return ""
	}
	tid := parts[1]
	if len(tid) != 32 {
		return ""
	}
	for i := 0; i < len(tid); i++ {
		c := tid[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	// All-zero trace_id is invalid per W3C trace-context.
	if tid == "00000000000000000000000000000000" {
		return ""
	}
	return tid
}
