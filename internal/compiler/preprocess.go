package compiler

import (
	"strings"

	"github.com/zeebo/blake3"
)

// PreprocessResult is everything downstream needs after the preprocessor
// runs: the preprocessed source bytes, their BLAKE3-256 digest (so the
// hasher doesn't recompute it), the compiler's stderr, and its exit code.
//
// A non-zero ExitCode is NOT returned as a Go error — the preprocessor
// failing (e.g. missing header, syntax error) is a normal cacheable
// outcome, not an hpcc-side error. A non-nil error means hpcc itself
// couldn't run the binary (path missing, exec failure).
type PreprocessResult struct {
	Source   []byte
	Digest   [32]byte
	Stderr   []byte
	ExitCode int
}

// runCompilerCmd dispatches bin+args+extraEnv via the given Executor.
// Thin wrapper kept so call sites read uniformly across the package; the
// real work lives in the Executor implementation (LocalExecutor for the
// host path, a Task.Exec-backed executor on the worker side).
func runCompilerCmd(e Executor, bin string, args, extraEnv []string) (stdout, stderr []byte, exitCode int, err error) {
	return e.Run(bin, args, extraEnv)
}

// runPreprocessor invokes bin with args via the given Executor, capturing
// preprocessed source (stdout), stderr, and exit code. The BLAKE3-256
// digest of the source is computed once during this call.
func runPreprocessor(e Executor, bin string, args []string) (*PreprocessResult, error) {
	stdout, stderr, exitCode, err := runCompilerCmd(e, bin, args, nil)
	if err != nil {
		return nil, err
	}
	return &PreprocessResult{
		Source:   stdout,
		Digest:   blake3.Sum256(stdout),
		Stderr:   stderr,
		ExitCode: exitCode,
	}, nil
}

// parseMakeDeps parses GCC/Clang -M output (Makefile-style dependency
// rules) into a flat list of paths. The first ":"-separated token is the
// target ("foo.o:") and gets dropped; everything after is dependencies,
// possibly across many continuation-backslash lines.
//
// Doesn't handle filenames containing whitespace (Make escapes those with
// "\ "). In real C/C++ projects this never comes up.
func parseMakeDeps(data []byte) []string {
	s := string(data)
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ReplaceAll(s, "\\\r\n", " ")
	s = strings.ReplaceAll(s, "\\\n", " ")
	return strings.Fields(s)
}

// parseShowIncludes parses cl.exe /showIncludes output (on stderr). Lines
// look like:
//
//	Note: including file: C:\path\to\header.h
//	Note: including file:  C:\nested.h     (extra spaces indicate depth)
//
// The English prefix is forced via VSLANG=1033 — without it the prefix is
// localized and parsing breaks.
func parseShowIncludes(data []byte) []string {
	const prefix = "Note: including file:"
	var deps []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		path := strings.TrimSpace(line[len(prefix):])
		if path != "" {
			deps = append(deps, path)
		}
	}
	return deps
}

// filterSources removes anything in `sources` from `deps`, preserving order.
// Used to keep FindDependencies' return value consistent across compiler
// families: GNU's -M lists the source file as a dep, MSVC's /showIncludes
// doesn't. Excluding it everywhere means the hasher gets the same shape.
func filterSources(deps, sources []string) []string {
	if len(sources) == 0 {
		return deps
	}
	skip := make(map[string]struct{}, len(sources))
	for _, s := range sources {
		skip[s] = struct{}{}
	}
	out := deps[:0]
	for _, d := range deps {
		if _, drop := skip[d]; !drop {
			out = append(out, d)
		}
	}
	return out
}

// stripGNUModeAndOutput drops -c/-E/-S/-M/-MM and -o (with its value) from
// args, preserving everything else in original order. Used to build a
// preprocess-only command line from an invocation that may have been a
// compile or link.
func stripGNUModeAndOutput(args []string) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			out = append(out, a)
			i++
			continue
		}
		spec, _, consumed, ok := matchGNUFlag(args, i)
		if ok && (spec.Category == CatMode || spec.Category == CatOutput) {
			i += consumed
			continue
		}
		out = append(out, a)
		i++
	}
	return out
}

// stripMSVCModeAndOutput is the cl.exe counterpart of stripGNUModeAndOutput.
func stripMSVCModeAndOutput(args []string) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		if !isMSVCFlag(a) {
			out = append(out, a)
			i++
			continue
		}
		spec, _, consumed, ok := matchMSVCFlag(args, i)
		if ok && (spec.Category == CatMode || spec.Category == CatOutput) {
			i += consumed
			continue
		}
		out = append(out, a)
		i++
	}
	return out
}
