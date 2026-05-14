package compiler

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aarani/hpcc/internal/enum"
	"github.com/zeebo/blake3"
)

type clCompiler struct {
	name string   // always "cl"
	path string   // argv[0] as the user supplied it
	exec Executor // LocalExecutor by default; worker swaps in Task.Exec
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
	return runPreprocessor(c.exec, c.path, args, inv.Cwd)
}

func (c *clCompiler) FindDependencies(inv *Invocation) ([]string, error) {
	// MSVC has no -M equivalent. /showIncludes prints "Note: including
	// file: ..." lines to stderr during preprocess/compile. Combine it
	// with /E so we don't actually produce object code, /nologo to drop
	// the banner, and VSLANG=1033 so the prefix is English (otherwise
	// it's localized and the parser misses every line).
	args := append([]string{"/E", "/showIncludes", "/nologo"},
		stripMSVCModeAndOutput(inv.RawArgs)...)
	_, stderr, exitCode, err := runCompilerCmd(c.exec, c.path, args, []string{"VSLANG=1033"}, inv.Cwd)
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
	start := time.Now()
	stdout, stderr, exitCode, err := runCompilerCmd(c.exec, c.path, inv.RawArgs, nil, inv.Cwd)
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
		data, err := c.exec.ReadOutput(inv.Output)
		if err != nil {
			return nil, fmt.Errorf("read output %q: %w", inv.Output, err)
		}
		result.Output = data
	}
	return result, nil
}

func (c *clCompiler) RewriteForPreprocessed(inv *Invocation, srcPath string) (*Invocation, error) {
	langFlag := msvcPreprocessedLanguage(inv)
	return rewriteMSVCForPreprocessed(inv, srcPath, langFlag), nil
}

func (c *clCompiler) Identity() ([]byte, error) {
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
