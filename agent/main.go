// hpcc-agent is the in-VM PID-1 inside every per-tenant Firecracker
// microVM. The rootfs is mounted read-only and the agent owns
// everything writable (tmpfs at /tmp, /run, /dev/shm, plus the
// per-Exec staging tree under /run/hpcc), so this binary has to act
// as a minimal Linux init: bring kernel filesystems up, set up
// scratch tmpfses, then sit on a signal loop reaping zombies the way
// any well-behaved init does.
//
// The vsock RPC server that dispatches compiles into the guest is
// intentionally NOT here yet. This file is the bootstrap; downstream
// work plugs the RPC handler in alongside reapLoop.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := setupInit(); err != nil {
		log.Fatalf("hpcc-agent: init: %v", err)
	}
	go reapLoop()

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
		log.Printf("hpcc-agent: received %s, exiting", sig)
	case err := <-errCh:
		log.Fatalf("hpcc-agent: vsock server: %v", err)
	}
}
