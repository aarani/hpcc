package runner

import (
	"fmt"
	"os"

	"github.com/aarani/hpcc/internal/cache"
	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
)

// NewContext detects the requested compiler, loads the config (from the
// path in HPCC_CONFIG, or the default user-config-dir location), wires up
// the configured cache stores behind a V1Cache facade, and returns a
// ready-to-use Context.
//
// A missing config file is fine — defaults apply. A malformed one is
// surfaced as an error so a typo isn't silently ignored.
//
// This lives in the runner package, not compiler, because constructing
// a cache.V1Cache requires importing the cache package, which itself
// depends on compiler types — putting NewContext in compiler would form
// an import cycle.
func NewContext(compilerName string) (*compiler.Context, error) {
	path := os.Getenv("HPCC_CONFIG")
	if path == "" {
		p, err := config.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return nil, err
	}

	c, err := compiler.Detect(compilerName)
	if err != nil {
		return nil, fmt.Errorf("detect %q: %w", compilerName, err)
	}

	stores, err := store.FromConfig(cfg.Caches)
	if err != nil {
		return nil, err
	}

	ctx := &compiler.Context{Compiler: c, Config: &cfg}
	ctx.Cache = cache.NewV1Cache(ctx, stores)
	return ctx, nil
}
