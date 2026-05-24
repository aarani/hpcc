// hpcc-agent is the in-VM PID-1 inside every per-tenant Firecracker
// microVM. The rootfs is mounted read-only and the agent owns
// everything writable (tmpfs at /tmp, /run, /dev/shm, plus the
// per-Exec staging tree under /run/hpcc), so this binary has to act
// as a minimal Linux init: bring kernel filesystems up, set up
// scratch tmpfses, then serve the vsock RPC.
//
// There is no zombie-reaping signal loop here: compiles run in their
// own process group (see exec_linux.go) so cmd.Wait reaps the gcc
// driver and runCompiler's defer sweeps any helpers gcc didn't reap
// before exiting. Nothing else can reparent to us, so PID 1 has no
// orphans to collect.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	logger := initLogger()
	defer func() { _ = logger.Sync() }()
	_ = zap.RedirectStdLog(logger)

	if err := setupInit(); err != nil {
		zap.S().Fatalf("hpcc-agent: init: %v", err)
	}

	// Ensure PATH is set on the agent process itself so exec.Command
	// can resolve bare compiler names ("gcc", "cc", …) via LookPath.
	// LookPath reads os.Getenv("PATH") from the CURRENT process, not
	// from any cmd.Env we set on the child — so a child-env-only
	// default isn't enough. The kernel passes init=/.hpcc/agent with
	// a near-empty environment (often just HOME=/), which would make
	// every bare-name compile fail with "executable file not found in
	// $PATH" inside the guest. Set our own PATH if the kernel didn't.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}

	// Run the gRPC server on a goroutine and surface its errors via
	// errCh; a Serve return is fatal — the VMM will reap us and the
	// runner will get a clear "agent died" rather than a silently
	// hung Exec.
	errCh := make(chan error, 1)
	go func() { errCh <- serveAgent() }()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigs:
		zap.S().Infof("hpcc-agent: received %s, exiting", sig)
	case err := <-errCh:
		zap.S().Fatalf("hpcc-agent: vsock server: %v", err)
	}
}

// initLogger installs and returns a console-format zap logger writing
// to stderr (which goes to the VM's kernel console). The agent is a
// tiny in-VM PID-1, so it doesn't import the main module's
// internal/logging package — it carries its own minimal setup.
func initLogger() *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encCfg),
		zapcore.Lock(os.Stderr),
		zap.InfoLevel,
	)
	logger := zap.New(core)
	zap.ReplaceGlobals(logger)
	return logger
}
