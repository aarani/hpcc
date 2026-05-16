//go:build !linux && !windows

package main

import "errors"

// setupInit is a no-go on dev hosts (macOS, BSDs) — the agent is
// only ever executed inside a Linux microVM or a Windows container.
// Stub keeps `go build ./...` happy from a Mac dev machine.
func setupInit() error {
	return errors.New("hpcc-agent: only supported on linux (PID-1 init) and windows (container entrypoint)")
}
