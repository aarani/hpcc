package compiler

import (
	"errors"
	"fmt"

	"github.com/aarani/hpcc/internal/enum"
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
	return nil, errors.New("clang.Invoke: not implemented")
}

func (c *clangCompiler) Identity() ([]byte, error) {
	return nil, errors.New("clang.Identity: not implemented")
}
