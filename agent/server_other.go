//go:build !linux && !windows

package main

import "errors"

// stagingRoot has to be defined for the cross-platform server.go code
// to compile on this platform, even though serveAgent below errors
// out before any of that code runs. Pick a Linux-style path: on
// darwin / freebsd it's a notional location that never gets created.
const stagingRoot = "/run/hpcc"

// serveAgent is unreachable in normal use — main bails earlier in
// setupInit. Stub exists so dev builds (macOS, BSDs) still link.
func serveAgent() error {
	return errors.New("hpcc-agent: server only supported on linux (vsock) and windows (hvsock)")
}
