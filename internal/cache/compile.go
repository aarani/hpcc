package cache

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
)

// Blob names used by CompileCache. They match the conventional names documented
// on store.Store so a single key directory is interpretable across cache
// implementations and out-of-band tools.
const (
	blobOutput   = "output"
	blobStdout   = "stdout"
	blobStderr   = "stderr"
	blobExitCode = "exit_code"
	blobMetadata = "metadata"
	// blobExtras holds the encodeExtras-serialised side-effect
	// outputs (InvocationResult.Extras). Only present when the
	// compile produced any — typically .d files from -Wp,-MMD,X in
	// CAS mode. Cache hits replay this so the client gets a fresh .d
	// file even without re-running gcc. Format is the tiny
	// length-prefixed binary defined in encodeExtras (no base64
	// overhead vs JSON for the binary payload).
	blobExtras = "extras"
)

// CompileCache is the compile-output cache facade. It wraps an ordered
// list of Stores and treats them as an L1/L2/... chain: Lookup queries
// each in order and returns the first hit; Store writes to all of them.
// Missing or corrupted entries in any one store are skipped, not
// surfaced as errors — a partial layer should not block other layers.
//
// CompileCache owns the "compile" namespace within each underlying
// store. Pass raw stores from store.FromConfig; the constructor calls
// .Namespace("compile") on each so on-disk layout becomes
// <root>/compile/<hh>/<full-hex>/<blob> (and the S3 equivalent under
// cache/compile/...). Source blobs and manifests live under sibling
// namespaces owned by other facades.
type CompileCache struct {
	ctx    *compiler.Context
	stores []store.Store
}

// CompileCacheBackend is the contract CompileCache satisfies. Kept as
// a same-package assertion target rather than an externally-used
// interface — callers reach CompileCache through compiler.CacheBackend
// (the mirror in the consumer package, which exists to break the
// import cycle that prevents the cache package from being imported
// here). Naming it CompileCache-specific avoids implying that future
// sibling facades (SourceStore, ManifestStore) will share this shape
// — they won't, since they key on raw content digests rather than
// parsed invocations.
//
// tenantID is an explicit per-call parameter rather than a constructor
// arg: one CompileCache instance backs every tenant on a shared
// worker/daemon, but each Lookup/Store binds to one tenant for the
// duration of that call. See docs/plan/multi-tenant.md "Storage
// isolation". Callers with no tenant context (the runner-only local
// fast path) pass "local".
type CompileCacheBackend interface {
	Lookup(inv *compiler.Invocation, tenantID string) (*compiler.InvocationResult, error)
	Store(inv *compiler.Invocation, res *compiler.InvocationResult, tenantID string) error
}

// TenantLocal is the sentinel tenant used by the local-only runner
// path (no daemon, no remote). A single developer's machine has no
// meaningful namespace neighbours to isolate from; pinning to a
// constant keeps the on-disk layout shape uniform across runner,
// daemon, and worker so tooling that walks the cache tree doesn't
// have to special-case "no-tenant" entries.
const TenantLocal = "local"

var _ CompileCacheBackend = (*CompileCache)(nil)

// metadata is the JSON shape written under the "metadata" blob. It is
// purely informational — none of these fields participate in the cache
// key, so they can change between CompileCache writes without invalidating
// older entries.
type metadata struct {
	Timestamp  time.Time `json:"timestamp"`
	Compiler   string    `json:"compiler"`
	Command    []string  `json:"command"`
	Inputs     []string  `json:"inputs"`
	Output     string    `json:"output,omitempty"`
	DurationNS int64     `json:"duration_ns"`
}

// NewCompileCache returns a CompileCache wrapping the given stores,
// each namespaced under "compile". The caller retains ownership of
// ctx; CompileCache holds it so it can derive cache keys (which
// require the compiler's identity and the config's preprocessing
// mode).
func NewCompileCache(ctx *compiler.Context, stores []store.Store) *CompileCache {
	namespaced := make([]store.Store, len(stores))
	for i, s := range stores {
		namespaced[i] = s.Namespace("compile")
	}
	return &CompileCache{ctx: ctx, stores: namespaced}
}

// Lookup returns a hit (non-nil result, nil error) if any wrapped store
// has a complete entry for inv's cache key, or a miss (nil result, nil
// error) otherwise. On hit the cached output object is written to
// inv.Output before the result is returned, so the caller's contract
// with the user — that the output file exists at the requested path —
// holds whether the compile ran or was replayed.
func (c *CompileCache) Lookup(inv *compiler.Invocation, tenantID string) (*compiler.InvocationResult, error) {
	if len(c.stores) == 0 {
		return nil, nil
	}
	if tenantID == "" {
		return nil, fmt.Errorf("CompileCache.Lookup: tenantID is required")
	}
	key, err := inv.CacheKey(c.ctx)
	if err != nil {
		return nil, fmt.Errorf("cache key: %w", err)
	}

	for _, s := range c.stores {
		ts := s.Namespace(tenantID)
		has, err := ts.Has(key)
		if err != nil || !has {
			continue
		}
		res, ok, err := loadEntry(ts, key, inv.Output)
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
func (c *CompileCache) Store(inv *compiler.Invocation, res *compiler.InvocationResult, tenantID string) error {
	if len(c.stores) == 0 || res == nil {
		return nil
	}
	// Skip caching failed compiles. A non-zero exit may be a
	// deterministic compiler diagnostic (broken source, missing
	// header), an ephemeral failure (OOM kill, signal death, vsock
	// disconnect mid-compile surfaced as exit=-1), or a transient
	// worker-environment problem (missing toolchain package on the
	// rootfs that gets fixed by re-rolling the image). All three
	// look identical at this layer, and persisting any of them
	// poisons the entry until the cache is manually cleaned —
	// every subsequent build of the same TU replays the failure
	// without re-running the compiler. The tradeoff against ccache-
	// style "fast replay of deterministic errors" is intentional:
	// re-dispatching a known-bad TU to a worker is cheap, but
	// shipping a one-off bad result to every developer that hits
	// the shared cache is expensive to recover from.
	if res.ExitCode != 0 {
		return nil
	}
	if tenantID == "" {
		return fmt.Errorf("CompileCache.Store: tenantID is required")
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

	var extrasBlob []byte
	if len(res.Extras) > 0 {
		extrasBlob = encodeExtras(res.Extras)
	}

	for _, s := range c.stores {
		ts := s.Namespace(tenantID)
		if output != nil {
			if err := ts.Put(key, blobOutput, output); err != nil {
				return err
			}
		}
		if err := ts.Put(key, blobStdout, res.Stdout); err != nil {
			return err
		}
		if err := ts.Put(key, blobStderr, res.Stderr); err != nil {
			return err
		}
		if err := ts.Put(key, blobExitCode, exitCode); err != nil {
			return err
		}
		if err := ts.Put(key, blobMetadata, meta); err != nil {
			return err
		}
		if extrasBlob != nil {
			if err := ts.Put(key, blobExtras, extrasBlob); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadEntry pulls a complete cached entry from s. It returns ok=false
// (without an error) when the entry is incomplete or corrupted, so the
// caller can fall through to the next store rather than failing the
// whole Lookup. exit_code is the canary: a present-but-unparsable code
// signals a half-written entry written by an older or buggy implementation.
//
// outputPath is the path the *caller* expects the artifact to land at;
// loadEntry uses it only as a signal that the caller wants the output
// blob loaded. We populate res.Output with the bytes and leave it to
// the caller to materialize the file via whatever path translation it
// has — runner.Run and daemon.handleRequest already do `os.WriteFile
// (inv.Output, result.Output, …)` after Lookup returns, and the
// worker hit path forwards res.Output as the CompileResponse's
// OutputArtifact. An earlier version of this function wrote the file
// here, which silently broke FC-paranoid lookups: on the worker side
// outputPath is an in-VM staging path like /out/foo.o that doesn't
// exist on the host, so os.WriteFile failed, Lookup returned err,
// the worker treated every hit as a miss, and warm builds re-
// compiled every TU despite the cache being intact.
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

	res := &compiler.InvocationResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
	}

	if outputPath != "" {
		output, err := s.Get(key, blobOutput)
		if err != nil {
			return nil, false, fmt.Errorf("get output: %w", err)
		}
		if output == nil {
			// Caller expected an output for this entry but none was
			// stored — treat as a miss so the wrap path proceeds to
			// re-invoke the compiler and (we hope) writes a complete
			// entry on Store.
			return nil, false, nil
		}
		res.Output = output
	}

	// Side-effect outputs (.d files, etc.) — optional. An entry
	// stored before the extras blob was introduced, or one whose
	// compile didn't produce any, has no extras blob and that's fine.
	// Decode failures are treated as a miss rather than an error so
	// a future format bump (different blob name, different encoding)
	// doesn't poison the lookup path — the caller will recompile and
	// store a fresh entry under the same key.
	if extrasRaw, err := s.Get(key, blobExtras); err == nil && extrasRaw != nil {
		if extras, err := decodeExtras(extrasRaw); err == nil {
			res.Extras = extras
		}
	}

	return res, true, nil
}

// encodeExtras serializes a map[path][]byte to a flat
// length-prefixed binary blob. JSON would base64 every []byte (~33%
// overhead, and for kernel-tag .d files that's wire weight we don't
// want). Format per entry:
//
//	[path_len: uint32-BE][path bytes][value_len: uint64-BE][value bytes]
//
// Plus a leading [count: uint32-BE]. Entries are emitted in
// path-sorted order so the encoding is deterministic — two compiles
// that produced the same extras hash to the same bytes, which keeps
// the cache layer from churning under map-iteration noise. Decoder
// is symmetric and validates lengths.
func encodeExtras(extras map[string][]byte) []byte {
	paths := make([]string, 0, len(extras))
	for p := range extras {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Reasonable up-front size to avoid most growth reallocs.
	approx := 4
	for _, p := range paths {
		approx += 4 + len(p) + 8 + len(extras[p])
	}
	buf := make([]byte, 0, approx)
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(paths)))
	buf = append(buf, hdr[:4]...)
	for _, p := range paths {
		v := extras[p]
		binary.BigEndian.PutUint32(hdr[:4], uint32(len(p)))
		buf = append(buf, hdr[:4]...)
		buf = append(buf, p...)
		binary.BigEndian.PutUint64(hdr[:8], uint64(len(v)))
		buf = append(buf, hdr[:8]...)
		buf = append(buf, v...)
	}
	return buf
}

// decodeExtras parses the encodeExtras format. Returns an error on
// truncated input or impossible lengths; callers treat any error as
// "no extras" rather than poisoning the cache lookup.
func decodeExtras(data []byte) (map[string][]byte, error) {
	if len(data) < 4 {
		return nil, io.ErrUnexpectedEOF
	}
	count := binary.BigEndian.Uint32(data[:4])
	off := 4
	out := make(map[string][]byte, count)
	for range count {
		if len(data)-off < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		pl := binary.BigEndian.Uint32(data[off : off+4])
		off += 4
		if uint64(len(data)-off) < uint64(pl)+8 {
			return nil, io.ErrUnexpectedEOF
		}
		path := string(data[off : off+int(pl)])
		off += int(pl)
		vl := binary.BigEndian.Uint64(data[off : off+8])
		off += 8
		if uint64(len(data)-off) < vl {
			return nil, io.ErrUnexpectedEOF
		}
		val := append([]byte(nil), data[off:off+int(vl)]...)
		off += int(vl)
		out[path] = val
	}
	return out, nil
}
