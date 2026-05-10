package compiler

import "github.com/aarani/hpcc/internal/enum"

// Compiler
type Compiler interface {
	Name() string                                          // "gcc", "clang"
	Family() enum.Family                                   // GNU | MSVC
	Parse(args []string) (*Invocation, error)              // argv -> structured form
	Preprocess(inv *Invocation) (*PreprocessResult, error) // run -E, capture source+digest+stderr
	Invoke(inv *Invocation) (*InvocationResult, error)     // run the actual compile
	FindDependencies(inv *Invocation) ([]string, error)    // run -M, capture dependency file list
	Identity() ([]byte, error)                             // path + binary hash, for cache key

	// RewriteForPreprocessed returns a copy of inv whose argv compiles
	// the preprocessed source at srcPath. Includes, defines, and other
	// preprocess-only flags are stripped (preprocessing already inlined
	// them); the input file is replaced with srcPath; -x cpp-output (or
	// the C++ equivalent) is added so the compiler skips its own
	// preprocessor. Used client-side when shipping a PreprocessedDescriptor.
	RewriteForPreprocessed(inv *Invocation, srcPath string) (*Invocation, error)
}
