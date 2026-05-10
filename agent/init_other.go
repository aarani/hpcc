//go:build !linux

package main

import "errors"

// setupInit is a no-go on non-Linux — devtmpfs/devpts/proc are
// Linux-specific and the agent only ever runs as PID 1 inside a
// Linux microVM. The stub exists so `go build ./...` from a Mac dev
// machine still type-checks the module.
func setupInit() error {
	return errors.New("hpcc-agent: only supported on linux (PID-1 init)")
}
