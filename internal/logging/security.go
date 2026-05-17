package logging

import "go.uber.org/zap"

// Field keys attached to every Security() entry. Kept as constants so
// log-pipeline filters can match them verbatim without drifting.
const (
	FieldCategory = "category"
	FieldEvent    = "event"
	FieldSeverity = "severity"

	CategorySecurity = "security"
	SeverityCritical = "critical"
)

// Security logs a misbehaving-client event at ERROR level with a
// structured `category=security` / `severity=critical` pair so log
// pipelines can filter (and alert on) the audit-relevant subset. The
// `event` is a short kebab-case identifier downstream alerting can
// group on (e.g. "auth-failed", "token-tenant-mismatch",
// "manifest-digest-mismatch"); msg is a one-line human summary.
//
// Call this at every site where a request from an untrusted peer
// fails authentication, fails authorization, fails an integrity
// check, or otherwise looks like attempted abuse. Include enough
// structured context (remote address, tenant_id, worker_id, claimed
// digest, etc.) that an operator reading one entry knows who did
// what — but never include the raw secret value being checked.
func Security(event, msg string, fields ...zap.Field) {
	base := []zap.Field{
		zap.String(FieldCategory, CategorySecurity),
		zap.String(FieldEvent, event),
		zap.String(FieldSeverity, SeverityCritical),
	}
	zap.L().Error(msg, append(base, fields...)...)
}
