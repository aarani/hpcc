//go:build !linux

// Windows containers and dev hosts don't have the
// PID-1-reaps-orphaned-children responsibility that Linux microVMs
// do. Windows container processes are tracked by the host's HCS
// (or by the OS itself outside a container), and exec.Cmd's
// process-handle lifetime handles cleanup for us. So reapLoop is a
// no-op everywhere except Linux.

package main

func reapLoop() {}
