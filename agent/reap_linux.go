//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// reapLoop drains zombies reparented to PID 1. The compiler driver
// (gcc/clang) forks helpers (cc1, as, ld); when one of those exits
// before its parent waits, the kernel reparents the zombie to us and
// we have to call wait4 to collect it or its slot leaks until the
// guest hits the pid_max ceiling.
//
// SIGCHLD deliveries coalesce — multiple children exiting between
// two of our wait4 calls produce one signal — so each notification
// has to drain in a WNOHANG loop until no more children are waiting.
func reapLoop() {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	for range sigchld {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}
