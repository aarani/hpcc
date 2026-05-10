package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aarani/hpcc/internal/worker/runtime"
	"github.com/google/uuid"
)

// runtimeExecutor implements compiler.Executor by dispatching each
// compiler invocation as a Task.Exec inside a per-tenant container. One
// instance is built per Compile RPC: ctx, container, and the per-RPC
// source/output mount paths are all bound at construction. The compiler
// package has no awareness of this; SetExecutor swaps it in at the
// start of the Compile handler.
type runtimeExecutor struct {
	ctx       context.Context
	container runtime.Container

	// srcHostPath / outHostPath are the per-RPC host directories the
	// runtime should expose at /src and /out for the compiler's Execs.
	// Plumbed into every ExecRequest — pooled containers serve many
	// compiles, each with its own pair of tmpdirs.
	srcHostPath string
	outHostPath string

	// inContainerOut is the in-container mount point ReadOutput strips
	// before joining onto outHostPath. Defaults to "/out".
	inContainerOut string
}

func newRuntimeExecutor(ctx context.Context, c runtime.Container, srcHostPath, outHostPath string) *runtimeExecutor {
	return &runtimeExecutor{
		ctx:            ctx,
		container:      c,
		srcHostPath:    srcHostPath,
		outHostPath:    outHostPath,
		inContainerOut: "/out",
	}
}

// Run implements compiler.Executor. It builds an ExecRequest from the
// compiler's bin+args+env and forwards it to the container. A non-zero
// exit code is returned as data; only dispatch failures (container dead,
// shim error, ctx cancelled) come back as err.
func (r *runtimeExecutor) Run(bin string, args, extraEnv []string) (stdout, stderr []byte, exitCode int, err error) {
	var so, se bytes.Buffer

	id, err := uuid.NewV7()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("generate exec id: %w", err)
	}

	res, err := r.container.Exec(r.ctx, runtime.ExecRequest{
		ExecID:      id.String(),
		Argv:        append([]string{bin}, args...),
		Env:         extraEnv,
		SrcHostPath: r.srcHostPath,
		OutHostPath: r.outHostPath,
		Stdout:      &so,
		Stderr:      &se,
	})
	if err != nil {
		return so.Bytes(), se.Bytes(), -1, fmt.Errorf("container exec %q: %w", bin, err)
	}
	return so.Bytes(), se.Bytes(), res.ExitCode, nil
}

// ReadOutput translates an in-container path (e.g. "/out/foo.o") to the
// host side of the output bind mount and reads the bytes. Paths outside
// the output mount are rejected — the compiler should never ask for
// anything outside /out, and silently reading from elsewhere would mask
// bugs and broaden the trust boundary.
func (r *runtimeExecutor) ReadOutput(path string) ([]byte, error) {
	clean := filepath.ToSlash(filepath.Clean(path))
	prefix := r.inContainerOut + "/"
	if !strings.HasPrefix(clean, prefix) && clean != r.inContainerOut {
		return nil, fmt.Errorf("ReadOutput: path %q is outside the container output mount %q", path, r.inContainerOut)
	}
	rel := strings.TrimPrefix(clean, prefix)
	return os.ReadFile(filepath.Join(r.outHostPath, rel))
}
