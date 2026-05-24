//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the compiler driver and any helpers it forks
// (cc1, as, ld) into their own process group, so a single
// kill(-pgid, SIGKILL) tears down the whole tree on ctx cancellation
// instead of leaving the helpers running and reparenting them to us
// as PID 1 zombies once gcc dies.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup signals every process in cmd's group. Wired in as
// cmd.Cancel so the os/exec context-watch goroutine calls it the
// instant the RPC stream ctx fires.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// reapProcessGroup drains any zombies still parked in cmd's process
// group once cmd.Wait has returned. The happy path is empty: gcc
// exits cleanly after reaping cc1/as/ld itself. The path that matters
// is gcc dying before it reaps them — abnormal exit on cancellation
// or a crash — where the helpers reparent to PID 1 as zombies. Their
// stdio fds have already been closed by the time cmd.Wait returned
// (that's how it detected EOF on the pipes), so they're zombies, not
// runners; a WNOHANG sweep collects them without blocking.
func reapProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}
