package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func clangAvailable(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not found in PATH")
	}
	return path
}

func writeSource(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClangInvoke_compilesToObject(t *testing.T) {
	clangPath := clangAvailable(t)
	c := &clangCompiler{name: "clang", path: clangPath, exec: LocalExecutor{}}

	dir := t.TempDir()
	src := writeSource(t, dir, "hello.c", "int main(void) { return 0; }\n")
	out := filepath.Join(dir, "hello.o")

	inv := &Invocation{
		Inputs:  []string{src},
		Output:  out,
		RawArgs: []string{"-c", src, "-o", out},
	}

	result, err := c.Invoke(inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if len(result.Output) == 0 {
		t.Fatal("expected non-empty object file bytes in result.Output")
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestClangInvoke_capturesCompileError(t *testing.T) {
	clangPath := clangAvailable(t)
	c := &clangCompiler{name: "clang", path: clangPath, exec: LocalExecutor{}}

	dir := t.TempDir()
	src := writeSource(t, dir, "bad.c", "this is not valid C;\n")
	out := filepath.Join(dir, "bad.o")

	inv := &Invocation{
		Inputs:  []string{src},
		Output:  out,
		RawArgs: []string{"-c", src, "-o", out},
	}

	result, err := c.Invoke(inv)
	if err != nil {
		t.Fatalf("Invoke should not return Go error for compile failure: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("expected non-zero exit code for invalid source")
	}
	if len(result.Stderr) == 0 {
		t.Error("expected diagnostic output on stderr")
	}
	if result.Output != nil {
		t.Error("expected nil Output for failed compile")
	}
}

func TestClangInvoke_stdoutPassthrough(t *testing.T) {
	clangPath := clangAvailable(t)
	c := &clangCompiler{name: "clang", path: clangPath, exec: LocalExecutor{}}

	dir := t.TempDir()
	src := writeSource(t, dir, "asm.c", "int x = 42;\n")

	inv := &Invocation{
		Inputs:  []string{src},
		RawArgs: []string{"-S", "-emit-llvm", "-o", "-", src},
	}

	result, err := c.Invoke(inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if len(result.Stdout) == 0 {
		t.Error("expected LLVM IR on stdout")
	}
}

func TestClangIdentity_deterministic(t *testing.T) {
	clangPath := clangAvailable(t)
	c := &clangCompiler{name: "clang", path: clangPath, exec: LocalExecutor{}}

	id1, err := c.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	id2, err := c.Identity()
	if err != nil {
		t.Fatalf("Identity (2nd call): %v", err)
	}
	if len(id1) == 0 {
		t.Fatal("expected non-empty identity")
	}
	if string(id1) != string(id2) {
		t.Error("Identity not deterministic across calls")
	}
}

func TestClangIdentity_missingBinary(t *testing.T) {
	c := &clangCompiler{name: "clang", path: "/no/such/clang", exec: LocalExecutor{}}
	_, err := c.Identity()
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

// TestLocalExecutor_forwardsStdin pins the behaviour the Linux kernel
// (and other build systems) rely on: when the compiler reads source
// from "-", the wrapper must pass its own stdin through. Without
// this, scripts/cc-version.sh's preprocess-a-heredoc probe sees an
// empty file, no __clang__/__GNUC__ macros are emitted, and the
// build surfaces a misleading "unknown C compiler" error.
func TestLocalExecutor_forwardsStdin(t *testing.T) {
	clangPath := clangAvailable(t)

	const probe = `#if defined(__clang__)
Clang
#elif defined(__GNUC__)
GCC
#else
unknown
#endif
`
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	go func() {
		defer w.Close()
		_, _ = w.Write([]byte(probe))
	}()

	stdout, stderr, exitCode, err := LocalExecutor{}.Run(
		clangPath, []string{"-E", "-P", "-x", "c", "-"}, nil, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("clang exit %d; stderr=%q", exitCode, stderr)
	}
	out := string(stdout)
	if !strings.Contains(out, "Clang") && !strings.Contains(out, "GCC") {
		t.Fatalf("stdin not forwarded — preprocessor saw an empty file; got %q", out)
	}
}
