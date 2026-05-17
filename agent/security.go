package main

import "go.uber.org/zap"

// Mirror of internal/logging.Security() inside the agent module. The
// agent is a separate Go module and can't import internal/logging
// directly, so we duplicate the structure here: every "client behaved
// badly" event lands at ERROR level with `category=security`,
// `severity=critical`, and an `event=<kebab-case>` tag the host's log
// pipeline can filter on uniformly across both modules.
//
// The agent's "client" is the worker host over vsock. The host is
// nominally trusted, but a path-traversal or malformed-frame attempt
// reaching the agent indicates either a bug on the host side or a
// compromised host process — both worth surfacing prominently.
const (
	fieldCategory = "category"
	fieldEvent    = "event"
	fieldSeverity = "severity"

	categorySecurity = "security"
	severityCritical = "critical"
)

func securityEvent(event, msg string, fields ...zap.Field) {
	base := []zap.Field{
		zap.String(fieldCategory, categorySecurity),
		zap.String(fieldEvent, event),
		zap.String(fieldSeverity, severityCritical),
	}
	zap.L().Error(msg, append(base, fields...)...)
}
