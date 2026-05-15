//go:build linux

package main

import (
	"syscall"
	"time"
)

// reapInterval is how often reapLoop wakes up to drain orphan
// zombies. Long enough that we don't burn CPU on a busy compile
// host, short enough that pid slots from a misbehaved compile chain
// drain well before pid_max becomes an issue (32K on most distros,
// often higher in CI).
const reapInterval = 5 * time.Second

// reapLoop drains zombies reparented to PID 1. The compiler driver
// (gcc/clang) forks helpers (cc1, as, ld); if a parent exits before
// it reaps a child, the kernel reparents that zombie to us and we
// have to call wait4 to collect it or its pid slot leaks.
//
// The naïve version subscribed to SIGCHLD and called Wait4(-1, …)
// on every notification. That races destructively with Go's
// exec.Cmd: when runCompiler does cmd.Run/cmd.Wait, the Go runtime
// also wants to wait on the compiler subprocess's pid, and the
// kernel only delivers a zombie's exit status to ONE waiter. If
// reapLoop wins the race the compiler's pid is reaped before
// cmd.Wait() reaches it, and Go reports
// "waitid: no child processes" — every loss surfaces as a remote
// dispatch failure that falls back to a local compile, defeating
// the entire FC dispatch path on a heavy workload like the kernel.
//
// The fix: don't subscribe to SIGCHLD. Let Go's runtime own waits
// for processes Go started (it tracks them by specific pid and
// reaps them via cmd.Wait). reapLoop instead polls every few
// seconds with Wait4(-1, WNOHANG) to catch the rare truly-orphan
// case — a grandchild whose intermediate parent exited before
// reaping it. WNOHANG returns immediately when no zombies exist,
// so this is cheap. Polling can still race with a Go-owned child
// that exited within the same poll window, but the window is now
// ~5 seconds wide vs ~immediate, and Go's wait nearly always wins
// because it's actively blocked on Wait4(specific_pid).
func reapLoop() {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for range ticker.C {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}
