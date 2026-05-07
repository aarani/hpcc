package compiler

import (
	"errors"
	"fmt"

	"github.com/aarani/hpcc/internal/enum"
)

type clCompiler struct {
	name string // always "cl"
	path string // argv[0] as the user supplied it
}

func (c *clCompiler) Name() string        { return c.name }
func (c *clCompiler) Family() enum.Family { return enum.MSVCFamily }

func (c *clCompiler) Parse(args []string) (*Invocation, error) {
	return ParseMSVC(args)
}

func (c *clCompiler) Preprocess(inv *Invocation) (*PreprocessResult, error) {
	// /nologo suppresses cl.exe's banner so preprocessed output stays clean.
	// Harmless if the user already passed it.
	args := append([]string{"/E", "/nologo"}, stripMSVCModeAndOutput(inv.RawArgs)...)
	return runPreprocessor(c.path, args)
}

func (c *clCompiler) FindDependencies(inv *Invocation) ([]string, error) {
	// MSVC has no -M equivalent. /showIncludes prints "Note: including
	// file: ..." lines to stderr during preprocess/compile. Combine it
	// with /E so we don't actually produce object code, /nologo to drop
	// the banner, and VSLANG=1033 so the prefix is English (otherwise
	// it's localized and the parser misses every line).
	args := append([]string{"/E", "/showIncludes", "/nologo"},
		stripMSVCModeAndOutput(inv.RawArgs)...)
	_, stderr, exitCode, err := runCompilerCmd(c.path, args, []string{"VSLANG=1033"})
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("cl /showIncludes exit %d: %s", exitCode, stderr)
	}
	// /showIncludes already excludes the source file itself; filterSources
	// is a no-op here but kept for symmetry with the GNU side.
	return filterSources(parseShowIncludes(stderr), inv.Inputs), nil
}

func (c *clCompiler) Invoke(inv *Invocation) (*InvocationResult, error) {
	return nil, errors.New("cl.Invoke: not implemented")
}

func (c *clCompiler) Identity() ([]byte, error) {
	return nil, errors.New("cl.Identity: not implemented")
}
