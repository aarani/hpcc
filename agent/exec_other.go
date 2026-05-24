//go:build !linux

// Non-Linux builds (Windows containers, dev hosts) don't have the
// PID-1-reaps-orphaned-children responsibility that Linux microVMs do,
// and Setpgid isn't available on Windows anyway. The default
// cmd.Cancel (Process.Kill) is sufficient there.

package main

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func reapProcessGroup(cmd *exec.Cmd) {}
