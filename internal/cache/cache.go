package cache

import "github.com/aarani/hpcc/internal/compiler"

// Cache is the interface for a single cache backend. The cache loop will
// call Lookup with the compiler invocation; if it returns a hit, the loop
// will replay the cached result. If it returns a miss, the loop will invoke
// the compiler and then call Store with the result.
type Cache interface {
	Lookup(inv *compiler.Invocation) (*compiler.InvocationResult, error)
	Store(inv *compiler.Invocation, res *compiler.InvocationResult) error
}
