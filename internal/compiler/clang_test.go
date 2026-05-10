package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
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
