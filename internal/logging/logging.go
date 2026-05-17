// Package logging configures the project-wide zap logger.
//
// Every binary (daemon, scheduler, worker, bench/fcstack) calls Init
// once during startup. The returned *zap.Logger is also installed as
// the package-global via zap.ReplaceGlobals, so package-level call
// sites can use zap.L() / zap.S() without plumbing a logger through.
//
// Environment overrides:
//
//   - HPCC_LOG_LEVEL: debug | info | warn | error | dpanic | panic | fatal
//     (default: info)
//   - HPCC_LOG_FORMAT: console | json (default: console)
//
// Output goes to stderr, matching the historical stdlib log behaviour.
package logging

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Init installs and returns the process-wide zap logger. Safe to call
// multiple times; the most recent call wins.
func Init() *zap.Logger {
	level := zap.NewAtomicLevelAt(zap.InfoLevel)
	if s := os.Getenv("HPCC_LOG_LEVEL"); s != "" {
		var lvl zapcore.Level
		if err := lvl.UnmarshalText([]byte(s)); err == nil {
			level.SetLevel(lvl)
		}
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder

	var enc zapcore.Encoder
	if os.Getenv("HPCC_LOG_FORMAT") == "json" {
		enc = zapcore.NewJSONEncoder(encCfg)
	} else {
		enc = zapcore.NewConsoleEncoder(encCfg)
	}

	core := zapcore.NewCore(enc, zapcore.Lock(os.Stderr), level)
	logger := zap.New(core)
	zap.ReplaceGlobals(logger)
	return logger
}
