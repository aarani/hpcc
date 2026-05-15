package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
)

// Blob names used by V1Cache. They match the conventional names documented
// on store.Store so a single key directory is interpretable across cache
// implementations and out-of-band tools.
const (
	blobOutput   = "output"
	blobStdout   = "stdout"
	blobStderr   = "stderr"
	blobExitCode = "exit_code"
	blobMetadata = "metadata"
)

// V1Cache is the first-generation compiler cache facade. It wraps an
// ordered list of Stores and treats them as an L1/L2/... chain: Lookup
// queries each in order and returns the first hit; Store writes to all
// of them. Missing or corrupted entries in any one store are skipped,
// not surfaced as errors — a partial layer should not block other layers.
type V1Cache struct {
	ctx    *compiler.Context
	stores []store.Store
}

var _ Cache = (*V1Cache)(nil)

// metadata is the JSON shape written under the "metadata" blob. It is
// purely informational — none of these fields participate in the cache
// key, so they can change between V1Cache writes without invalidating
// older entries.
type metadata struct {
	Timestamp  time.Time `json:"timestamp"`
	Compiler   string    `json:"compiler"`
	Command    []string  `json:"command"`
	Inputs     []string  `json:"inputs"`
	Output     string    `json:"output,omitempty"`
	DurationNS int64     `json:"duration_ns"`
}

// NewV1Cache returns a V1Cache wrapping the given stores. The caller
// retains ownership of ctx; V1Cache holds it so it can derive cache
// keys (which require the compiler's identity and the config's
// preprocessing mode).
func NewV1Cache(ctx *compiler.Context, stores []store.Store) *V1Cache {
	return &V1Cache{ctx: ctx, stores: stores}
}

// Lookup returns a hit (non-nil result, nil error) if any wrapped store
// has a complete entry for inv's cache key, or a miss (nil result, nil
// error) otherwise. On hit the cached output object is written to
// inv.Output before the result is returned, so the caller's contract
// with the user — that the output file exists at the requested path —
// holds whether the compile ran or was replayed.
func (c *V1Cache) Lookup(inv *compiler.Invocation) (*compiler.InvocationResult, error) {
	if len(c.stores) == 0 {
		return nil, nil
	}
	key, err := inv.CacheKey(c.ctx)
	if err != nil {
		return nil, fmt.Errorf("cache key: %w", err)
	}

	for _, s := range c.stores {
		has, err := s.Has(key)
		if err != nil || !has {
			continue
		}
		res, ok, err := loadEntry(s, key, inv.Output)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		return res, nil
	}
	return nil, nil
}

// Store writes the result of a freshly-completed compile to every
// wrapped store. The output bytes are taken from res.Output when
// populated (the executor — LocalExecutor on the daemon-host path,
// runtimeExecutor on the worker-in-VM path — has already read them
// via its own ReadOutput, which knows how to resolve in-VM staging
// paths back to host-side mount points). If res.Output is empty we
// fall back to reading inv.Output off disk; this covers callers
// that haven't populated res.Output yet (and the local fast-path,
// where inv.Output resolves correctly relative to the daemon's
// cwd anyway).
//
// The fallback's os.ReadFile is the source of a subtle bug on the
// worker side: inv.Output there is an in-VM path like /out/foo.o
// that doesn't exist on the host, so ReadFile returns ENOENT, the
// "tolerate missing" branch leaves output = nil, and the output
// blob is silently skipped — caches end up with metadata-only
// entries that report as misses on lookup. Preferring res.Output
// fixes that without needing to teach Store about the runtime
// path translation. A missing output is still tolerated (modes
// that don't produce a single output file simply skip the blob).
func (c *V1Cache) Store(inv *compiler.Invocation, res *compiler.InvocationResult) error {
	if len(c.stores) == 0 || res == nil {
		return nil
	}
	key, err := inv.CacheKey(c.ctx)
	if err != nil {
		return fmt.Errorf("cache key: %w", err)
	}

	var output []byte
	switch {
	case len(res.Output) > 0:
		output = res.Output
	case inv.Output != "":
		data, err := os.ReadFile(inv.Output)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read output %q: %w", inv.Output, err)
		}
		output = data
	}

	meta, err := json.Marshal(metadata{
		Timestamp:  time.Now().UTC(),
		Compiler:   c.ctx.Compiler.Name(),
		Command:    inv.RawArgs,
		Inputs:     inv.Inputs,
		Output:     inv.Output,
		DurationNS: res.Duration.Nanoseconds(),
	})
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	exitCode := []byte(strconv.Itoa(res.ExitCode))

	for _, s := range c.stores {
		if output != nil {
			if err := s.Put(key, blobOutput, output); err != nil {
				return err
			}
		}
		if err := s.Put(key, blobStdout, res.Stdout); err != nil {
			return err
		}
		if err := s.Put(key, blobStderr, res.Stderr); err != nil {
			return err
		}
		if err := s.Put(key, blobExitCode, exitCode); err != nil {
			return err
		}
		if err := s.Put(key, blobMetadata, meta); err != nil {
			return err
		}
	}
	return nil
}

// loadEntry pulls a complete cached entry from s. It returns ok=false
// (without an error) when the entry is incomplete or corrupted, so the
// caller can fall through to the next store rather than failing the
// whole Lookup. exit_code is the canary: a present-but-unparsable code
// signals a half-written entry written by an older or buggy implementation.
func loadEntry(s store.Store, key []byte, outputPath string) (*compiler.InvocationResult, bool, error) {
	exitCodeRaw, err := s.Get(key, blobExitCode)
	if err != nil {
		return nil, false, fmt.Errorf("get exit_code: %w", err)
	}
	if exitCodeRaw == nil {
		return nil, false, nil
	}
	exitCode, err := strconv.Atoi(string(exitCodeRaw))
	if err != nil {
		return nil, false, nil
	}

	stdout, err := s.Get(key, blobStdout)
	if err != nil {
		return nil, false, fmt.Errorf("get stdout: %w", err)
	}
	stderr, err := s.Get(key, blobStderr)
	if err != nil {
		return nil, false, fmt.Errorf("get stderr: %w", err)
	}

	if outputPath != "" {
		output, err := s.Get(key, blobOutput)
		if err != nil {
			return nil, false, fmt.Errorf("get output: %w", err)
		}
		if output == nil {
			return nil, false, nil
		}
		if err := os.WriteFile(outputPath, output, 0o644); err != nil {
			return nil, false, fmt.Errorf("write output %q: %w", outputPath, err)
		}
	}

	return &compiler.InvocationResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
	}, true, nil
}
