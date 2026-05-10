//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// reapLoop drains zombies reparented to PID 1. Multiple SIGCHLDs can
// coalesce into one delivery, so each notification has to drain in a
// WNOHANG loop until no more children are waiting.
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
