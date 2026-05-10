package compiler

import (
	"fmt"
	"strings"
)

// Detect picks a Compiler implementation based on argv[0]. The argument can
// be a bare command ("clang"), a relative path ("./clang.exe"), or absolute
// ("/usr/bin/clang") — only the basename (minus a ".exe" suffix) is used to
// dispatch.
//
// The returned Compiler stores argv0 verbatim. Resolution to a real binary
// path happens lazily in Invoke/Preprocess/Identity so callers (and tests)
// don't need the toolchain installed just to parse args.
func Detect(argv0 string) (Compiler, error) {
	name := normalizeName(argv0)
	switch name {
	case "clang", "clang++", "cc", "c++":
		// cc and c++ are the POSIX-named drivers — on Linux they're
		// usually GCC, on macOS they're Apple Clang. Either way the
		// argument grammar is GNU-flavored, which is all the wrapper
		// needs to know; the actual binary on PATH is what runs the
		// compile.
		return &clangCompiler{name: name, path: argv0, exec: LocalExecutor{}}, nil
	case "cl":
		return &clCompiler{name: name, path: argv0, exec: LocalExecutor{}}, nil
	}
	return nil, fmt.Errorf("unknown compiler %q", argv0)
}

// normalizeName strips directory components and a trailing ".exe" so dispatch
// works the same on every platform regardless of how the user invoked it.
// Handles both "/" and "\" separators so a Windows-style path passed on a
// Linux host (e.g. via a manifest) still resolves correctly.
func normalizeName(argv0 string) string {
	if i := strings.LastIndexAny(argv0, `/\`); i >= 0 {
		argv0 = argv0[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(argv0), ".exe")
}
