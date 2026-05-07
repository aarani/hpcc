package compiler

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aarani/hpcc/internal/enum"
	"github.com/zeebo/blake3"
)

type clangCompiler struct {
	name string // "clang" or "clang++"
	path string // argv[0] as the user supplied it
}

func (c *clangCompiler) Name() string        { return c.name }
func (c *clangCompiler) Family() enum.Family { return enum.GNUFamily }

func (c *clangCompiler) Parse(args []string) (*Invocation, error) {
	return ParseGNU(args)
}

func (c *clangCompiler) Preprocess(inv *Invocation) (*PreprocessResult, error) {
	args := append([]string{"-E"}, stripGNUModeAndOutput(inv.RawArgs)...)
	return runPreprocessor(c.path, args)
}

func (c *clangCompiler) FindDependencies(inv *Invocation) ([]string, error) {
	// -M is a mode flag (replaces -c/-E/-S) that emits Makefile-style deps
	// to stdout. Includes system headers; for cache keys this is correct.
	// Use -MM later if/when we want to elide system headers (e.g. when
	// the toolchain identity already pins them via image digest).
	args := append([]string{"-M"}, stripGNUModeAndOutput(inv.RawArgs)...)
	stdout, stderr, exitCode, err := runCompilerCmd(c.path, args, nil)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("clang -M exit %d: %s", exitCode, stderr)
	}
	return filterSources(parseMakeDeps(stdout), inv.Inputs), nil
}

func (c *clangCompiler) Invoke(inv *Invocation) (*InvocationResult, error) {
	start := time.Now()
	stdout, stderr, exitCode, err := runCompilerCmd(c.path, inv.RawArgs, nil)
	if err != nil {
		return nil, err
	}
	result := &InvocationResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
		Duration: time.Since(start),
	}
	if inv.Output != "" && exitCode == 0 {
		data, err := os.ReadFile(inv.Output)
		if err != nil {
			return nil, fmt.Errorf("read output %q: %w", inv.Output, err)
		}
		result.Output = data
	}
	return result, nil
}

func (c *clangCompiler) Identity() ([]byte, error) {
	path, err := exec.LookPath(c.path)
	if err != nil {
		return nil, fmt.Errorf("resolve compiler path: %w", err)
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read compiler binary: %w", err)
	}
	h := blake3.New()
	h.Write(bin)
	return h.Sum(nil), nil
}
