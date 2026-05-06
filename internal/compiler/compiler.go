package compiler

import "github.com/aarani/hpcc/internal/enum"

// Compiler
type Compiler interface {
	Name() string                                      // "gcc", "clang"
	Family() enum.Family                               // GNU | MSVC
	Parse(args []string) (*Invocation, error)          // argv -> structured form
	Preprocess(inv *Invocation) ([]byte, error)        // run -E
	Invoke(inv *Invocation) (*InvocationResult, error) // run the actual compile
	Identity() (string, error)                         // path + binary hash, for cache key
}
