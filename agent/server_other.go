//go:build !linux

package main

import "errors"

// serveAgent is unreachable in normal use — main bails earlier in
// setupInit. Stub exists so non-Linux builds (developer macOS) still
// link.
func serveAgent() error {
	return errors.New("hpcc-agent: vsock server only supported on linux")
}
