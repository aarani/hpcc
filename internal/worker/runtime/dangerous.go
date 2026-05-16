package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aarani/hpcc/internal/protocol/gen"
)

// HandlerReallyReallyDangerous is the config.toml runtime.handler value
// that selects DangerouslyExecOnHost. The string is intentionally awful
// so nobody sets it without knowing what they're doing — every compile
// runs as a child process of the worker, on the worker host, with the
// worker's full file system, network, and credentials. There is no
// kernel boundary, no NIC removal, no audit envelope. Development only.
const HandlerReallyReallyDangerous = "really_really_dangerous"

// DangerouslyExecOnHost is a Runtime implementation that does NOT
// isolate compiles at all — every Exec is forked from the worker
// process via os/exec, sharing the worker's PID namespace, file system,
// network namespace, and uid. It exists solely to exercise the
// worker→runtime call path during development; selecting it in
// production puts the entire worker host inside the trust boundary.
//
// The /src and /out "mounts" are simulated by string-substituting the
// in-container paths in argv with the host-side paths from
// ContainerSpec at exec time. From the rest of the system's
// perspective (worker handler, runtimeExecutor, compiler package),
// argv looks the same as it would for a real Firecracker run.
type DangerouslyExecOnHost struct{}

func (DangerouslyExecOnHost) Start(ctx context.Context, spec ContainerSpec) (Container, error) {
	return &dangerousContainer{spec: spec}, nil
}

func (DangerouslyExecOnHost) Close() error { return nil }

type dangerousContainer struct {
	spec ContainerSpec
}

func (c *dangerousContainer) ID() string          { return c.spec.ID }
func (c *dangerousContainer) TenantID() string    { return c.spec.TenantID }
func (c *dangerousContainer) ImageDigest() string { return c.spec.ImageDigest }

// State always reports RUNNING — there's no VM lifecycle to track. If
// the worker keeps a stopped container in its active map and asks for
// state, returning RUNNING is the least surprising answer (the
// alternative would be to add a STOPPED enum value, which the rest of
// the system doesn't need).
func (c *dangerousContainer) State() gen.VMState { return gen.VMState_RUNNING }

// Exec translates argv from in-container shape to host shape and forks
// a child process. ctx cancellation propagates to the child via
// CommandContext; the child gets SIGKILL.
func (c *dangerousContainer) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if len(req.Argv) == 0 {
		return ExecResult{ExitCode: -1}, fmt.Errorf("Exec: empty argv")
	}

	tx := func(s string) string { return translateExecPath(s, req.SrcHostPath, req.OutHostPath) }

	argv := make([]string, len(req.Argv))
	for i, a := range req.Argv {
		argv[i] = tx(a)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = req.Env
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	switch {
	case req.Cwd != "":
		cmd.Dir = tx(req.Cwd)
	case req.SrcHostPath != "":
		cmd.Dir = req.SrcHostPath
	}

	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ExecResult{ExitCode: ee.ExitCode()}, nil
		}
		return ExecResult{ExitCode: -1}, fmt.Errorf("danger-runtime exec %q: %w", argv[0], err)
	}
	return ExecResult{ExitCode: 0}, nil
}

// Stop is a no-op — the child process exited when Exec returned and
// there's no VM to reap. Idempotent.
func (c *dangerousContainer) Stop(_ context.Context) error { return nil }

// translateExecPath rewrites in-container path roots (/src, /out) to
// the per-Exec host-side mount paths. Boundary-aware: matches /src
// only when followed by "/" or end-of-string, so "/src-extra/x"
// doesn't turn into "<host>-extra/x".
func translateExecPath(s, srcHost, outHost string) string {
	if srcHost != "" {
		s = rewriteRoot(s, "/src", srcHost)
	}
	if outHost != "" {
		s = rewriteRoot(s, "/out", outHost)
	}
	return s
}

// rewriteRoot replaces the FIRST occurrence of root in s with
// replacement, but only when root sits on a path boundary. Stops
// after one match so that a CAS-mode argv like "/src/src/main.c"
// maps to "<host>/src/main.c" (one translation) rather than
// "<host><host>/main.c" (two). The leftmost /src is always the
// in-container root marker; any subsequent /src inside the path is
// a project-relative directory whose literal bytes must survive
// the rewrite. One pass, no regex; mirrors
// compiler.RewritePathPrefix's algorithm but kept local to avoid a
// runtime → compiler dependency.
//
// Path-boundary character set:
//
//   - end-of-string (the path was the whole tail of the argv element).
//   - "/" — standard POSIX separator (`-I/src/include`).
//   - "=" — flag-payload delimiter for GNU's `-ffile-prefix-map=A=B`
//     and similar (`-foo=/src=...`). Without this, the GNU
//     reproducibility flag injection would emit `/src` literal
//     into the compile environment where the in-container path
//     never appears, and `.obj` paths would silently include the
//     per-Exec staging directory.
//
// `=` deliberately excludes broader Sep-like characters (`,`, `:`)
// because those routinely appear inside paths the compiler sees
// (linker `-Wl,...` group separators, Windows drive colons), and
// false-matching them would mangle valid argv elements.
func rewriteRoot(s, root, replacement string) string {
	if !strings.Contains(s, root) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	matched := false
	for i := 0; i < len(s); {
		if !matched && strings.HasPrefix(s[i:], root) {
			end := i + len(root)
			if end == len(s) || isPathBoundary(s[end]) {
				b.WriteString(replacement)
				i = end
				matched = true
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// isPathBoundary reports whether c terminates a path prefix for the
// purposes of rewriteRoot. See rewriteRoot's doc comment for the
// rationale on the specific set.
func isPathBoundary(c byte) bool {
	return c == '/' || c == '='
}
