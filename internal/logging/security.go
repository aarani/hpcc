package logging

import (
	"sync/atomic"

	"go.uber.org/zap"
)

// Field keys attached to every Security() entry. Kept as constants so
// log-pipeline filters can match them verbatim without drifting.
const (
	FieldCategory = "category"
	FieldEvent    = "event"
	FieldSeverity = "severity"

	CategorySecurity = "security"
	SeverityCritical = "critical"
)

// SecurityHook is an optional callback fired by Security() in addition
// to the zap entry. internal/metrics registers one to mirror events
// into a counter; the agent module (which can't import internal/)
// just leaves it nil. event is the kebab-case identifier; fields are
// the same fields passed to Security so the hook can pull
// `tenant_id` (or any other dimension) out by key.
type SecurityHook func(event string, fields []zap.Field)

var securityHook atomic.Value // SecurityHook

// SetSecurityHook installs (or clears, with nil) the callback fired
// alongside every Security() entry. Safe to call before or after
// other Security calls; the most recent value wins.
func SetSecurityHook(h SecurityHook) {
	if h == nil {
		securityHook.Store(SecurityHook(nil))
		return
	}
	securityHook.Store(h)
}

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

	if v := securityHook.Load(); v != nil {
		if h, _ := v.(SecurityHook); h != nil {
			h(event, fields)
		}
	}
}
