package compiler

import (
	"github.com/aarani/hpcc/internal/config"
)

// CacheBackend is the structural shape of a cache facade. It lives in the
// compiler package — rather than importing the cache package directly —
// because the cache package needs to reference Invocation/InvocationResult,
// and pulling cache into compiler would form an import cycle. Any concrete
// cache (e.g. cache.V1Cache) satisfies this interface via duck typing.
type CacheBackend interface {
	Lookup(inv *Invocation) (*InvocationResult, error)
	Store(inv *Invocation, res *InvocationResult) error
}

// Context bundles everything the runner / cache layer needs about a
// single invocation: which compiler is being wrapped, the loaded config
// that controls cache/dispatch behavior, and the cache facade.
type Context struct {
	Config   *config.Config
	Compiler Compiler
	Cache    CacheBackend

	// IdentityOverride, when non-nil, replaces Compiler.Identity() in
	// cache-key derivation. Worker-side compiles set this to the image
	// digest of the toolchain container — the toolchain isn't on the
	// worker host's filesystem (Compiler.Identity reads ./clang etc.),
	// and the image digest is what actually pins the toolchain version
	// for cache-key purposes anyway.
	IdentityOverride []byte
}
