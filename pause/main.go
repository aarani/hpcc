// hpcc-pause is the entrypoint injected into every prepared image so the
// container's PID 1 stays alive between Task.Exec calls. It blocks on
// SIGTERM/SIGINT and, on Linux, reaps any children reparented to PID 1.
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	go reapLoop()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
