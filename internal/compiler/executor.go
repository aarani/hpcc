package compiler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

// Executor abstracts how a compiler invocation is dispatched. The host-
// local path uses os/exec; the worker-side path will use containerd's
// Task.Exec to run the same argv inside a per-tenant microVM. Both
// shapes return stdout, stderr, and exit code as data — a non-nil err
// means hpcc itself couldn't dispatch (binary missing, containerd
// unreachable, etc.); a non-zero exit code is normal cacheable data.
type Executor interface {
	Run(bin string, args, extraEnv []string) (stdout, stderr []byte, exitCode int, err error)

	// ReadOutput returns the bytes of a file produced by a previous Run.
	// Local: os.ReadFile from the host fs. Task.Exec: read from the
	// host-side mount of the container's output volume (§4.4 /out).
	ReadOutput(path string) ([]byte, error)
}

// LocalExecutor runs binaries on the host with os/exec and reads files
// from the host filesystem. This is the default for the wrapper / daemon
// path; the worker swaps in a containerd-backed executor.
type LocalExecutor struct{}

func (LocalExecutor) Run(bin string, args, extraEnv []string) (stdout, stderr []byte, exitCode int, err error) {
	var so, se bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &so
	cmd.Stderr = &se
	// Forward the wrapper's own stdin to the compiler. Several
	// real-world callers feed source through "-" on stdin: the
	// Linux kernel's scripts/cc-version.sh preprocesses a heredoc
	// via `$CC -E -P -x c -` to detect __GNUC__/__clang__, and
	// `echo … | cc -E -` is the canonical preprocessor probe. With
	// stdin unset the child sees an empty file, the probe parses
	// no macros, and the surrounding build prints a misleading
	// "unknown C compiler" diagnostic. Forwarding is a no-op for
	// invocations that don't read stdin — exec.Cmd closes the pipe
	// when Run returns.
	cmd.Stdin = os.Stdin
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	err = cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return so.Bytes(), se.Bytes(), ee.ExitCode(), nil
		}
		return so.Bytes(), se.Bytes(), -1, fmt.Errorf("run %q: %w", bin, err)
	}
	return so.Bytes(), se.Bytes(), 0, nil
}

func (LocalExecutor) ReadOutput(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// SetExecutor configures the Executor for compilers returned by Detect.
// The wrapper/daemon path leaves the default LocalExecutor in place; the
// worker side calls this with a Task.Exec-backed executor before
// dispatching a compile. No-op for compilers not built by this package.
func SetExecutor(c Compiler, e Executor) {
	switch cc := c.(type) {
	case *clangCompiler:
		cc.exec = e
	case *clCompiler:
		cc.exec = e
	}
}
