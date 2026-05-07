package compiler

import (
	"fmt"
	"os"

	"github.com/aarani/hpcc/internal"
)

// Context bundles everything the runner / cache layer needs about a
// single invocation: which compiler is being wrapped, and the loaded
// config that controls cache/dispatch behavior.
type Context struct {
	Compiler Compiler
	Config   internal.Config
}

// NewContext detects the requested compiler, loads the config (from the
// path in HPCC_CONFIG, or the default user-config-dir location), and
// returns a ready-to-use Context.
//
// A missing config file is fine — defaults apply. A malformed one is
// surfaced as an error so a typo isn't silently ignored.
func NewContext(compilerName string) (*Context, error) {
	path := os.Getenv("HPCC_CONFIG")
	if path == "" {
		p, err := internal.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := internal.LoadConfig(path)
	if err != nil {
		return nil, err
	}

	c, err := Detect(compilerName)
	if err != nil {
		return nil, fmt.Errorf("detect %q: %w", compilerName, err)
	}

	return &Context{Compiler: c, Config: cfg}, nil
}
