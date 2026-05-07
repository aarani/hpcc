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
}
