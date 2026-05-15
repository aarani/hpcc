package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	// Register the zstd compressor with the gRPC encoding registry so the
	// worker's gRPC server accepts and emits zstd-compressed messages when
	// peers ask for it via grpc-encoding / grpc-accept-encoding.
	_ "github.com/mostynb/go-grpc-compression/zstd"

	"github.com/aarani/hpcc/internal/cache"
	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker/image"
	"github.com/aarani/hpcc/internal/worker/runtime"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/zeebo/blake3"
)

// heartbeatInterval is how often the worker pushes state to the scheduler.
// Short enough that the scheduler's view of free capacity isn't stale,
// long enough that thousands of workers don't drown a single scheduler.
const heartbeatInterval = 10 * time.Second

// imageEntry is the per-digest record in Worker.Images. lastUsed is
// updated on every ensureImage call for the digest; the idle-image
// eviction loop drops entries (and untags the prepared image) once
// lastUsed is older than Image.IdleTimeout.
type imageEntry struct {
	lastUsed atomic.Int64 // unix nanos
}

func (e *imageEntry) touch() { e.lastUsed.Store(time.Now().UnixNano()) }

type Worker struct {
	Config Config
	// Images is the local image catalog: digest → *imageEntry. The
	// presence of an entry means "the prepared image for this digest
	// is locally available." Pulls add entries on success; advertised
	// and previously-prepared digests pre-populate at bootstrap.
	Images     sync.Map
	// ImageStore prepares per-tenant images for the runtime. nil means
	// "no image store" — the dev-mode dangerous runtime takes that
	// path: ensureImage records every digest as locally-present
	// without I/O, and the eviction loop only drops catalogue
	// entries. Real deployments wire either a cdimage.Store
	// (containerd, Windows under hcsshim) or a rootfs.Store
	// (Linux under raw Firecracker).
	ImageStore image.Store
	Containers sync.Map // containerID -> runtime.Container

	// imagePulls dedupes concurrent ensureImage calls for the same
	// digest so a thundering herd of compiles for an unfamiliar
	// toolchain only triggers one PullImage.
	imagePulls singleflight.Group

	// inflight is the count of compiles currently in flight across all
	// per-tenant VMs. A single VM can absorb VM.VCPUs concurrent
	// Task.Execs, so worker-wide capacity is MaxActive * VM.VCPUs and
	// each in-flight compile consumes one slot regardless of which VM
	// it lands in.
	inflight atomic.Int32

	workerID        string
	certFingerprint []byte // sha256 of the worker's serving cert; clients pin this

	sessionMu       sync.RWMutex
	sessionToken    string
	schedulerPubKey []byte // ed25519 pubkey for verifying client-presented task JWTs
	scheduler       gen.SchedulerServiceClient
	schedulerConn   *grpc.ClientConn

	runtime runtime.Runtime

	// caches is the worker-side cache backend list, built once from
	// Config.Caches. A fresh CompileCache is wrapped around these per Compile
	// RPC because CompileCache holds a *compiler.Context reference that's
	// only valid for one invocation.
	caches []store.Store

	// sourceStore is the worker-local namespace for CAS source blobs.
	// Initialized from the first disk-typed entry in Config.Caches,
	// namespaced to "source" so it sits next to the "compile" entries
	// under the same on-disk root without colliding. nil when no disk
	// cache is configured — in that case CAS-mode RPCs
	// (FindMissingBlobs / UploadBlobs) fail and the client falls back
	// to PREPROCESSED. The CompileCache path is unaffected.
	sourceStore store.Store

	gen.UnimplementedWorkerServiceServer
}

// blobData is the named slot under each source-blob entry's key in
// sourceStore. The Store interface keys are (digest, name); the
// digest discriminates which blob, the name discriminates which
// slot. Source blobs only have the one slot — the bytes themselves
// — but the store API still requires a name, and a fixed constant
// keeps the call sites unambiguous about what they're touching.
const blobData = "data"

var _ gen.WorkerServiceServer = (*Worker)(nil)

func NewDefaultWorker() *Worker {
	path, err := DefaultConfigPath()
	if err != nil {
		panic(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		panic(err)
	}

	w, err := NewWorker(cfg)
	if err != nil {
		panic(err)
	}
	return w
}

func NewWorker(cfg Config) (*Worker, error) {
	caches, err := store.FromConfig(cfg.Caches)
	if err != nil {
		return nil, fmt.Errorf("init worker caches: %w", err)
	}
	// Pick the first disk-typed cache to back the CAS source-blob
	// namespace. CompileCache will namespace its own view of the same
	// underlying disk store to "compile"; we take "source". S3 caches
	// are skipped — source blobs are worker-local-ephemeral by design
	// (see docs/cas.md Step 4). Nil when no disk cache is configured.
	var sourceStore store.Store
	for i, c := range cfg.Caches {
		if c.Type == enum.CacheDisk {
			sourceStore = caches[i].Namespace("source")
			break
		}
	}
	rt, err := runtime.Select(cfg.Runtime.Handler, runtime.Options{
		Firecracker: runtime.FirecrackerOptions{
			FirecrackerBin: cfg.Runtime.Firecracker.FirecrackerBin,
			JailerBin:      cfg.Runtime.Firecracker.JailerBin,
			KernelImage:    cfg.Runtime.Firecracker.KernelImage,
			RootfsDir:      cfg.Runtime.Firecracker.RootfsDir,
			RunDir:         cfg.Runtime.Firecracker.RunDir,
			UID:            cfg.Runtime.Firecracker.UID,
			GID:            cfg.Runtime.Firecracker.GID,
			BootArgs:       cfg.Runtime.Firecracker.BootArgs,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("init worker runtime: %w", err)
	}
	// Wrap the runtime in a per-tenant container pool so consecutive
	// compiles for the same (tenant, image) reuse a warm VM instead
	// of paying full boot cost each time. Empty idle_timeout disables
	// the idle reaper; empty session_timeout disables the hard
	// session ceiling (§4.2). Both empty → containers stay parked
	// until Close. Malformed values fail fast.
	var idleTTL time.Duration
	if cfg.VM.IdleTimeout != "" {
		d, err := cfg.VM.IdleTimeoutDur()
		if err != nil {
			return nil, fmt.Errorf("parse vm.idle_timeout: %w", err)
		}
		idleTTL = d
	}
	var sessionTTL time.Duration
	if cfg.VM.SessionTimeout != "" {
		d, err := cfg.VM.SessionTimeoutDur()
		if err != nil {
			return nil, fmt.Errorf("parse vm.session_timeout: %w", err)
		}
		sessionTTL = d
	}
	rt = runtime.NewPooledRuntime(rt, idleTTL, sessionTTL, cfg.Pool.MaxActive)
	return &Worker{Config: cfg, caches: caches, sourceStore: sourceStore, runtime: rt}, nil
}

func (w *Worker) Compile(ctx context.Context, req *gen.CompileRequest) (*gen.CompileResponse, error) {
	if req.Descriptor_ == nil {
		return nil, fmt.Errorf("missing CompileDescriptor")
	}

	if err := w.ValidateToken(req); err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}

	// Sanity-check: after client-side path rewriting, argv should
	// reference only in-container paths (/src, /out, system dirs). A
	// /home/, /Users/, or C:\Users\ leak means the client skipped or
	// botched the rewrite for the chosen source mode — fail loudly so
	// the bug surfaces here, not as a confusing in-VM "no such file."
	if err := compiler.ValidateNoHostPaths(req.Args); err != nil {
		return nil, fmt.Errorf("argv validation: %w", err)
	}

	containerID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate compile UUID: %w", err)
	}

	// CAS-mode: re-verify the manifest digest BEFORE doing any other
	// work for this request. We do this here (rather than next to the
	// ManifestDigest bind below) so a tampered or malformed manifest
	// is rejected before we pull an image, materialize blobs, or
	// start a container — none of which produce useful state for a
	// request we're about to refuse.
	var verifiedManifestDigest *[32]byte
	if cas := req.Descriptor_.GetCas(); cas != nil {
		md, err := verifyManifestDigest(cas)
		if err != nil {
			return nil, fmt.Errorf("verify manifest digest: %w", err)
		}
		verifiedManifestDigest = &md
	}

	// Make sure the prepared image for this digest is on the worker
	// before we ask the runtime to start a container against it. On a
	// miss this pulls (deduped across concurrent compiles for the same
	// digest); on a hit this just touches the lastUsed timestamp.
	if err := w.ensureImage(ctx, req.Descriptor_.ImageDigest, req.Descriptor_.ImageRef); err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	srcHostPath, outHostPath, cleanup, err := w.stageSource(req)
	if err != nil {
		return nil, fmt.Errorf("stage source: %w", err)
	}
	defer cleanup()

	// Pre-create parent directories for every /out/<path> the argv
	// mentions. Compilers (clang, gcc) refuse to create missing
	// parent dirs for output files — they open(...O_CREAT) the leaf
	// only. Today's PREPROCESSED path gets away with a single
	// /out/foo.o whose parent (outHostPath) always exists; CAS-mode
	// argv with -Wp,-MMD,/out/build/main.d needs /out/build/ to be
	// created first or clang errors with ENOENT before producing any
	// output. Substring-search across all argv elements catches the
	// joined -o<path>, separate -o <path>, and -Wp,-M*,<path> forms
	// uniformly without compiler-flag-aware parsing.
	if err := mkdirOutputParents(req.Args, outHostPath); err != nil {
		return nil, fmt.Errorf("prepare output dirs: %w", err)
	}

	// Pre-create every search-path directory the argv names that
	// resolves into the source-staging tree (-I/-iquote/-isystem/
	// -idirafter for header search, -L for library search). CAS only
	// stages files in the dep closure, so a search-path dir whose
	// contents aren't pulled in by this TU never gets materialised —
	// and gcc's -Werror=missing-include-dirs (kernel and many other
	// builds) and -Werror=missing-library-dirs fire before the
	// compile/link step even starts. Materialising the dir (empty if
	// needed) makes the flag-validation step pass without bloating
	// CAS with files we don't need.
	if err := mkdirSearchPaths(req.Args, srcHostPath); err != nil {
		return nil, fmt.Errorf("prepare search-path dirs: %w", err)
	}

	spec := runtime.ContainerSpec{
		ID:          containerID.String(),
		TenantID:    req.Descriptor_.TenantId,
		ImageDigest: req.Descriptor_.ImageDigest,
		VCPUs:       w.Config.VM.VCPUs,
		MemoryBytes: w.Config.VM.MemoryBytes(),
	}

	container, err := w.runtime.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	// Stop on a pooled runtime returns the container to the pool;
	// inside the dev runtime it's a no-op. Either way, defer is safe.
	defer container.Stop(ctx)

	c, err := compiler.Detect(req.Args[0])
	if err != nil {
		return nil, fmt.Errorf("detect compiler %q: %w", req.Args[0], err)
	}
	compiler.SetExecutor(c, newRuntimeExecutor(ctx, container, srcHostPath, outHostPath))

	inv, err := c.Parse(req.Args[1:])
	if err != nil {
		return nil, fmt.Errorf("compiler parse args: %w", err)
	}

	// For PREPROCESSED-mode requests we already have the preprocessed
	// bytes in hand; hand the digest straight to the cache-key path so
	// it doesn't try to re-run the preprocessor or read deps off the
	// host filesystem (in-container paths don't translate there).
	if pp := req.Descriptor_.GetPreprocessed(); pp != nil {
		d := blake3.Sum256(pp.PreprocessedSource)
		inv.PreprocessedDigest = &d
	}

	// For CAS-mode requests bind the (already-verified) manifest
	// digest to the invocation so CacheKey short-circuits to the
	// manifest-derived key. Without this rebind, CacheKey would fall
	// through to running the preprocessor — which would not work,
	// because the staged source paths only exist under /src
	// in-container.
	if verifiedManifestDigest != nil {
		inv.ManifestDigest = verifiedManifestDigest
	}

	cctx := w.compileContext(c, req.Descriptor_.ImageDigest)

	// Cache lookup is used in two cases:
	//   - Paranoid mode (PREPROCESSED or CAS): worker owns the cache,
	//     client never reads or writes it.
	//   - CAS mode (always): probe and Compile both consult the
	//     compile cache. Skipping the recheck here would leave a race
	//     window between probe miss and Compile where another tenant
	//     could populate the cache and we'd still compile.
	useCache := w.Config.Paranoid || req.Descriptor_.SourceMode == gen.SourceMode_CAS

	if useCache {
		if hit, err := cctx.Cache.Lookup(inv); err == nil && hit != nil {
			// Cache hit. hit.Extras was populated by loadEntry from
			// the cached `extras` blob, so the client gets the same
			// .d files the original cold compile produced — even
			// though no gcc ran this time.
			return w.respond(cctx, req, container, inv, hit), nil
		}
	}

	result, err := c.Invoke(inv)
	if err != nil {
		return nil, fmt.Errorf("invoke compiler: %w", err)
	}

	// Collect side-effect output files (typically .d files from
	// -MMD/-MD) that the client expects back. The agent has already
	// streamed everything from /out into outHostPath; walk it and
	// extract anything that isn't the primary -o artifact. The
	// client rewrote dep-emission paths to live under /out before
	// sending, so the relative key here matches the path it remembers.
	extras, err := collectExtraOutputs(outHostPath, inv.Output)
	if err != nil {
		return nil, fmt.Errorf("collect extra outputs: %w", err)
	}
	result.Extras = extras

	if useCache {
		// Store errors are non-fatal — we have the artifact, the next
		// request can recompute and try again. Extras go into the
		// cache too so warm hits replay the same .d files cold
		// compiles produced.
		_ = cctx.Cache.Store(inv, result)
	}

	return w.respond(cctx, req, container, inv, result), nil
}

// mkdirOutputParents walks argv looking for any substring matching
// "/out/<path>" and creates the host-side parent directory of <path>
// under outHostPath. The substring scan handles every shape the
// compiler driver uses: positional (-o /out/build/foo.o), joined
// (-o/out/build/foo.o), and embedded inside another flag's value
// (-Wp,-MMD,/out/build/foo.d). Stops at the first character that
// can't appear in a path (comma, whitespace, end-of-string) so a
// flag carrying multiple comma-separated values is segmented
// correctly.
//
// Idempotent: MkdirAll is a no-op when the dir already exists, so
// concurrent compiles writing to overlapping subtrees don't race.
func mkdirOutputParents(args []string, outHostPath string) error {
	if outHostPath == "" {
		return nil
	}
	const outPrefix = "/out/"
	for _, a := range args {
		rest := a
		for {
			idx := strings.Index(rest, outPrefix)
			if idx < 0 {
				break
			}
			tail := rest[idx+len(outPrefix):]
			// End of path = first char that can't legitimately appear
			// in a filesystem path argv element: comma (segments
			// -Wp,X,Y), whitespace (paranoia — argv shouldn't have
			// any but cheap to guard).
			end := strings.IndexAny(tail, ", \t")
			var rel string
			if end < 0 {
				rel = tail
				rest = ""
			} else {
				rel = tail[:end]
				rest = tail[end:]
			}
			if rel == "" {
				continue
			}
			parent := filepath.Dir(rel)
			if parent == "" || parent == "." {
				continue
			}
			full := filepath.Join(outHostPath, filepath.FromSlash(parent))
			if err := os.MkdirAll(full, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", full, err)
			}
		}
	}
	return nil
}

// searchPathFlags lists the argv flags whose value is a directory the
// compiler/linker will search for headers or libraries. Each entry is
// matched both as an exact argv element (separate form, value in the
// next slot) and as a prefix (joined form, value glued onto the same
// element). Order matters: longer prefixes are listed first so
// HasPrefix("-isystem", "-i") doesn't shadow them.
//
// Covers gcc's standard header search (-I, -iquote, -isystem,
// -idirafter — the set -Wmissing-include-dirs validates) and the
// linker's library search (-L — what -Wmissing-library-dirs
// validates). Add new flags here as builds turn up new failure modes;
// the rest of the helper doesn't care which family a flag belongs to.
var searchPathFlags = []string{
	"-idirafter",
	"-isystem",
	"-iquote",
	"-I",
	"-L",
}

// mkdirSearchPaths walks argv for compiler/linker search-path flags
// (see searchPathFlags) and ensures each named directory exists
// under srcHostPath. Handles both the joined (-Idir) and separate
// (-I dir) forms.
//
// Two path shapes get materialised:
//
//   - Absolute paths under /src/<rel>: stripped to <rel> and created
//     under srcHostPath. These come from RewriteForCAS rewriting the
//     project root.
//   - Relative paths (e.g. ./include/generated/uapi or include/foo):
//     created under srcHostPath as-is, since the compiler runs with
//     cwd = srcHostPath (dangerous runtime) or the in-VM equivalent.
//
// Everything else (system paths like /usr/include/foo) is skipped —
// those exist in the container image's rootfs, not in our staging
// tree, and creating them under srcHostPath would just confuse the
// header search later.
//
// Idempotent: MkdirAll is a no-op on existing dirs, so concurrent
// compiles whose search-path sets overlap don't race.
func mkdirSearchPaths(args []string, srcHostPath string) error {
	if srcHostPath == "" {
		return nil
	}
	const srcPrefix = "/src/"
	mkRel := func(rel string) error {
		if rel == "" || rel == "." {
			return nil
		}
		full := filepath.Join(srcHostPath, filepath.FromSlash(rel))
		if err := os.MkdirAll(full, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", full, err)
		}
		return nil
	}
	mkOne := func(p string) error {
		if p == "" {
			return nil
		}
		if strings.HasPrefix(p, srcPrefix) {
			return mkRel(p[len(srcPrefix):])
		}
		if p == "/src" {
			return nil
		}
		// Other absolute paths are system dirs the container image
		// provides; not our concern.
		if filepath.IsAbs(p) {
			return nil
		}
		return mkRel(p)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, f := range searchPathFlags {
			if a == f {
				if i+1 < len(args) {
					if err := mkOne(args[i+1]); err != nil {
						return err
					}
					i++
				}
				break
			}
			if strings.HasPrefix(a, f) {
				if err := mkOne(a[len(f):]); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// collectExtraOutputs walks outHostPath and returns every regular
// file it finds keyed by its path relative to outHostPath, EXCEPT
// the primary artifact named by inv.Output. The primary is excluded
// because it's already returned as CompileResponse.output_artifact;
// returning it twice would double-encode bytes on the wire.
//
// primaryInContainer is the inv.Output path as the compiler argv
// references it — e.g. "/out/scripts/mod/empty.o". The primary's
// relative-to-outHost key is everything after the "/out" prefix
// (or "out/" once translated by the runtime layer), so we just
// strip it and use that.
func collectExtraOutputs(outHostPath, primaryInContainer string) (map[string][]byte, error) {
	if outHostPath == "" {
		return nil, nil
	}
	primaryRel := strings.TrimPrefix(filepath.ToSlash(primaryInContainer), "/out/")
	extras := map[string][]byte{}
	err := filepath.WalkDir(outHostPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(outHostPath, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if key == primaryRel {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		extras[key] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(extras) == 0 {
		return nil, nil
	}
	return extras, nil
}

// workerCompileConfig is the worker-side stand-in Config every per-RPC
// compiler.Context points at. SourceMode=preprocessed because the
// worker has an in-VM preprocessor (via runtimeExecutor) — if
// anything ever reaches the cache-key default branch from here, the
// right tool is the preprocessor, not a host-side dep walk over
// paths that only exist inside the guest. In practice neither
// short-circuit path (PreprocessedDigest for PREPROCESSED,
// ManifestDigest for CAS) ever falls through to read this field;
// it's a defensive default.
var workerCompileConfig = &config.Config{SourceMode: enum.SourceModePreprocessed}

// compileContext bundles the compiler under test with the worker's
// persistent cache stores into a fresh *compiler.Context. A new one is
// built per RPC because CompileCache holds the Context by reference and the
// Context is single-invocation by design (different runtime executor,
// different request).
//
// IdentityOverride is set to the request's image digest: the toolchain
// runs inside the per-tenant VM, not on the worker host, so calling
// Compiler.Identity() (which reads the binary off the local fs) would
// either fail or — worse — silently hash an unrelated host-side compiler.
// The image digest is the right toolchain pin for cache-key purposes.
func (w *Worker) compileContext(c compiler.Compiler, imageDigest string) *compiler.Context {
	cctx := &compiler.Context{
		Compiler:         c,
		Config:           workerCompileConfig,
		IdentityOverride: []byte(imageDigest),
	}
	cctx.Cache = cache.NewCompileCache(cctx, w.caches)
	return cctx
}

// respond packages an InvocationResult into the wire-shape
// CompileResponse and attaches the per-job AuditRecord. Output bytes
// (already read off /out by the compiler driver via
// runtimeExecutor.ReadOutput) are forwarded as output_artifact;
// cache_key is best-effort (a hashing failure here just omits the
// field rather than failing the whole RPC, since the artifact is
// still valid).
//
// The audit record is also logged at the worker for operators who
// want a flat-file trail today; a durable sidecar sink is plan §4.12
// follow-up work.
func (w *Worker) respond(ctx *compiler.Context, req *gen.CompileRequest, container runtime.Container, inv *compiler.Invocation, r *compiler.InvocationResult) *gen.CompileResponse {
	cacheKey, _ := inv.ComputeHash(ctx)

	audit := w.buildAudit(req, container, inv, r, cacheKey)
	logAudit(audit)

	resp := &gen.CompileResponse{
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		ExitCode: int32(r.ExitCode),
		CacheKey: &cacheKey,
		Audit:    audit,
	}
	if len(r.Output) > 0 {
		resp.OutputArtifact = r.Output
	}
	// Same source on fresh compile (Extras set by collectExtraOutputs)
	// and on cache hit (Extras set by loadEntry from the cached blob).
	if len(r.Extras) > 0 {
		resp.ExtraOutputs = r.Extras
	}
	return resp
}

// buildAudit assembles the per-job AuditRecord required by §4.12.
// Most fields come straight off the request and the worker's identity
// snapshot; source_digest is only populated for PREPROCESSED-mode
// requests today (the only mode where the worker has a single canonical
// digest in hand) and stays nil for CAS until that staging mode
// lands. output_digest is the BLAKE3 of the artifact
// bytes the worker is about to return; an empty artifact (e.g. a
// compile that exited non-zero) yields a nil digest.
//
// scheduler_id is the scheduler URL the worker is currently
// authenticated against — that's the most stable identifier we have
// today; a real scheduler-issued ID can replace it without changing
// the wire shape.
func (w *Worker) buildAudit(req *gen.CompileRequest, container runtime.Container, inv *compiler.Invocation, r *compiler.InvocationResult, cacheKey string) *gen.AuditRecord {
	rec := &gen.AuditRecord{
		TenantId:    req.Descriptor_.TenantId,
		SchedulerId: w.Config.Scheduler.URL,
		WorkerId:    w.workerID,
		VmId:        container.ID(),
		ImageDigest: req.Descriptor_.ImageDigest,
		CacheKey:    cacheKey,
		Flags:       append([]string(nil), req.Args[1:]...),
		ExitCode:    int32(r.ExitCode),
		DurationMs:  r.Duration.Milliseconds(),
		Timestamp:   time.Now().UnixMilli(),
	}
	if inv.PreprocessedDigest != nil {
		rec.SourceDigest = append([]byte(nil), inv.PreprocessedDigest[:]...)
	}
	if len(r.Output) > 0 {
		sum := blake3.Sum256(r.Output)
		rec.OutputDigest = sum[:]
	}
	return rec
}

// logAudit prints the audit record as a single structured line so
// operators piping the worker's stdout to a log collector get a usable
// trail today. Expensive sinks (Kafka topic, signed log file) hook
// here in follow-up work.
func logAudit(rec *gen.AuditRecord) {
	log.Printf("audit tenant=%s worker=%s vm=%s image=%s cache_key=%s exit=%d duration_ms=%d source_digest=%x output_digest=%x",
		rec.TenantId, rec.WorkerId, rec.VmId, rec.ImageDigest,
		rec.CacheKey, rec.ExitCode, rec.DurationMs,
		rec.SourceDigest, rec.OutputDigest)
}

func (w *Worker) ValidateToken(req *gen.CompileRequest) error {
	if req.Descriptor_ == nil {
		return fmt.Errorf("missing descriptor")
	}
	return w.validateSchedulerToken(req.Descriptor_.SchedulerToken, req.Descriptor_.TenantId, req.Descriptor_.ImageDigest)
}

// validateSchedulerToken verifies the JWT against the scheduler's
// signing key and confirms its tenant_id / image_digest / worker_id
// claims match the request. Pulled out as a separate helper so both
// the Compile RPC (descriptor-shaped) and the CAS probe / upload
// RPCs (flat-shaped, no Descriptor wrapper) can share one
// implementation.
func (w *Worker) validateSchedulerToken(rawToken, tenantID, imageDigest string) error {
	// EdDSA verification expects an ed25519.PublicKey value, not a
	// raw []byte — the JWT lib type-switches on the exact type. Convert
	// at the boundary; ed25519.PublicKey is itself a []byte so this is a
	// type rename, not a copy.
	token, err := jwt.Parse(rawToken, func(token *jwt.Token) (any, error) {
		return ed25519.PublicKey(w.SchedulerSigningKey()), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil || !token.Valid {
		return fmt.Errorf("invalid scheduler token: %w", err)
	}

	claims := token.Claims.(jwt.MapClaims)
	if claims["tenant_id"] != tenantID {
		return fmt.Errorf("scheduler token tenant ID does not match request")
	}
	if claims["image_digest"] != imageDigest {
		return fmt.Errorf("scheduler token image digest does not match request")
	}
	if claims["worker_id"] != w.workerID {
		return fmt.Errorf("scheduler token worker ID does not match request")
	}

	return nil
}

// --- scheduler liaison ---------------------------------------------------

// Run drives the worker's scheduler-side loop: dial the scheduler,
// authenticate, register, then heartbeat on a ticker until ctx is
// cancelled. On RPC failures it re-authenticates rather than retrying
// the same dead session.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.bootstrap(); err != nil {
		return fmt.Errorf("bootstrap worker identity: %w", err)
	}

	if ttl, err := w.Config.Image.IdleTimeoutDur(); err != nil {
		return fmt.Errorf("parse image.idle_timeout: %w", err)
	} else if ttl > 0 {
		go w.evictImagesLoop(ctx, ttl)
	}

	if err := w.dialScheduler(ctx); err != nil {
		return fmt.Errorf("dial scheduler: %w", err)
	}
	defer w.schedulerConn.Close()

	if err := w.authenticateAndRegister(ctx); err != nil {
		return fmt.Errorf("initial auth+register: %w", err)
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.heartbeat(ctx); err != nil {
				log.Printf("worker: heartbeat failed: %v", err)
				// Most likely the scheduler restarted (signing key
				// changed) or our session was evicted. Re-auth and
				// re-register; the next tick will heartbeat anew.
				if rerr := w.authenticateAndRegister(ctx); rerr != nil {
					log.Printf("worker: re-auth after heartbeat failure: %v", rerr)
				}
			}
		}
	}
}

// bootstrap sets workerID and certFingerprint from config and seeds
// the image catalogue with whatever is already locally available
// (config-advertised digests + previously-prepared images in the
// snapshotter). Called once at the top of Run.
func (w *Worker) bootstrap() error {
	if w.workerID == "" {
		if w.Config.WorkerID != "" {
			w.workerID = w.Config.WorkerID
		} else {
			id, err := randomID()
			if err != nil {
				return fmt.Errorf("generate worker id: %w", err)
			}
			w.workerID = id
		}
	}

	fp, err := loadCertFingerprint(w.Config.TLS.CertFile)
	if err != nil {
		return fmt.Errorf("compute cert fingerprint: %w", err)
	}
	w.certFingerprint = fp

	for _, d := range w.Config.Image.AdvertisedDigests {
		entry := &imageEntry{}
		entry.touch()
		w.Images.LoadOrStore(d, entry)
	}
	if w.ImageStore != nil {
		existing, err := w.ImageStore.GetExistingImages(context.Background())
		if err != nil {
			log.Printf("worker: enumerate prepared images: %v", err)
		}
		for _, d := range existing {
			entry := &imageEntry{}
			entry.touch()
			w.Images.LoadOrStore(d, entry)
		}
	}
	return nil
}

func (w *Worker) dialScheduler(ctx context.Context) error {
	tlsCfg := &tls.Config{}
	if w.Config.Scheduler.CAFile != "" {
		pemBytes, err := os.ReadFile(w.Config.Scheduler.CAFile)
		if err != nil {
			return fmt.Errorf("read scheduler CA %q: %w", w.Config.Scheduler.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return fmt.Errorf("scheduler CA %q contains no PEM certs", w.Config.Scheduler.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	conn, err := grpc.NewClient(
		w.Config.Scheduler.URL,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		return err
	}
	w.schedulerConn = conn
	w.scheduler = gen.NewSchedulerServiceClient(conn)
	return nil
}

func (w *Worker) authenticateAndRegister(ctx context.Context) error {
	authResp, err := w.scheduler.Authenticate(ctx, &gen.AuthRequest{
		Token: &gen.AuthRequest_StaticToken{StaticToken: w.Config.Scheduler.WorkerToken},
	})
	if err != nil {
		return fmt.Errorf("Authenticate RPC: %w", err)
	}
	if !authResp.Success || authResp.SessionToken == "" {
		return fmt.Errorf("scheduler rejected worker token")
	}

	w.sessionMu.Lock()
	w.sessionToken = authResp.SessionToken
	w.schedulerPubKey = authResp.SigningPublicKey
	w.sessionMu.Unlock()

	avail, load := w.capacitySnapshot()
	regResp, err := w.scheduler.RegisterWorker(ctx, &gen.RegisterWorkerRequest{
		SessionToken:    authResp.SessionToken,
		WorkerId:        w.workerID,
		PublicAddr:      w.Config.PublicAddr,
		ImageDigests:    w.knownImageDigests(),
		AvailableVcpus:  avail,
		CurrentLoad:     load,
		Runtime:         w.runtimeType(),
		CertFingerprint: w.certFingerprint,
	})
	if err != nil {
		return fmt.Errorf("RegisterWorker RPC: %w", err)
	}
	if !regResp.Success {
		return fmt.Errorf("scheduler rejected RegisterWorker (error_code=%d)", regResp.ErrorCode)
	}
	return nil
}

func (w *Worker) heartbeat(ctx context.Context) error {
	w.sessionMu.RLock()
	session := w.sessionToken
	w.sessionMu.RUnlock()
	if session == "" {
		return fmt.Errorf("no session — must re-authenticate")
	}

	avail, load := w.capacitySnapshot()
	_, err := w.scheduler.Heartbeat(ctx, &gen.WorkerHeartbeat{
		SessionToken:   session,
		WorkerId:       w.workerID,
		AvailableVcpus: avail,
		CurrentLoad:    load,
		ActiveVms:      w.activeVMSnapshot(),
	})
	return err
}

// SchedulerSigningKey returns the scheduler's task-JWT signing pubkey
// learned during authentication. Compile RPCs use it to verify the JWT
// the client presents in gRPC metadata. Returns nil before first auth.
func (w *Worker) SchedulerSigningKey() []byte {
	w.sessionMu.RLock()
	defer w.sessionMu.RUnlock()
	return w.schedulerPubKey
}

// --- snapshots ----------------------------------------------------------

// capacitySnapshot returns (available_vcpus, current_load) for the
// scheduler. Each VM can run VM.VCPUs concurrent Task.Execs, so the
// worker's total compile budget is MaxActive*VCPUs slots; load is the
// number of in-flight compiles regardless of which VM they're in. The
// active_vms list (reported separately in the heartbeat) tells the
// scheduler which tenants this worker has warm VMs for, which is the
// signal that drives sticky-tenant routing.
func (w *Worker) capacitySnapshot() (avail, load int32) {
	load = w.inflight.Load()
	max := int32(w.Config.Pool.MaxActive) * w.Config.VM.VCPUs
	avail = max - load
	if avail < 0 {
		avail = 0
	}
	return avail, load
}

// BeginCompile / EndCompile are the hooks the Compile RPC handler uses
// to keep `inflight` in sync; defer EndCompile right after BeginCompile
// to avoid drift on early returns.
func (w *Worker) BeginCompile() { w.inflight.Add(1) }
func (w *Worker) EndCompile()   { w.inflight.Add(-1) }

func (w *Worker) activeVMSnapshot() []*gen.ActiveVM {
	var out []*gen.ActiveVM
	w.Containers.Range(func(_, val any) bool {
		c, ok := val.(runtime.Container)
		if !ok {
			return true
		}
		out = append(out, &gen.ActiveVM{
			VmId:        c.ID(),
			TenantId:    c.TenantID(),
			ImageDigest: c.ImageDigest(),
			State:       c.State(),
		})
		return true
	})
	return out
}

// knownImageDigests returns every image digest currently catalogued in
// w.Images. The catalogue is the source of truth for what this worker
// can serve cold-start-free; advertise it to the scheduler so routing
// can prefer warm workers (without excluding cold ones — a missing
// digest is now a cold-pull penalty in pickWorker, not a hard reject).
func (w *Worker) knownImageDigests() []string {
	var out []string
	w.Images.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		return true
	})
	return out
}

// ensureImage guarantees the prepared image for digest is locally
// available before the caller proceeds to runtime.Start. On a hit it's
// a touch on the entry's lastUsed timestamp. On a miss it pulls via
// the ImageStore (singleflight'd by digest so a herd of concurrent
// compiles only triggers one pull); when the worker has no real image
// store wired (e.g. the dangerous dev runtime), the digest is simply
// recorded as "have it" with no I/O.
func (w *Worker) ensureImage(ctx context.Context, digest, ref string) error {
	if entry, ok := w.Images.Load(digest); ok {
		entry.(*imageEntry).touch()
		return nil
	}

	_, err, _ := w.imagePulls.Do(digest, func() (any, error) {
		// Re-check inside the singleflight: another goroutine may
		// have completed the pull between our Load and our Do.
		if entry, ok := w.Images.Load(digest); ok {
			entry.(*imageEntry).touch()
			return nil, nil
		}

		if w.ImageStore != nil {
			if ref == "" {
				return nil, fmt.Errorf("image %s not present locally and no image_ref to pull from", digest)
			}
			log.Printf("worker: pulling image %s (ref=%s)", digest, ref)
			if err := w.ImageStore.PullImage(ctx, ref, digest); err != nil {
				return nil, fmt.Errorf("pull image %q: %w", ref, err)
			}
		}

		entry := &imageEntry{}
		entry.touch()
		w.Images.Store(digest, entry)
		return nil, nil
	})
	return err
}

// evictImagesLoop drops image-catalogue entries whose lastUsed is
// older than ttl, and untags the corresponding prepared image in
// containerd so its blobs become reclaimable. Runs at ttl/2 cadence so
// worst-case dwell after last use is at most 1.5×ttl. AdvertisedDigests
// are pinned (operator intent) and never evicted.
func (w *Worker) evictImagesLoop(ctx context.Context, ttl time.Duration) {
	pinned := make(map[string]struct{}, len(w.Config.Image.AdvertisedDigests))
	for _, d := range w.Config.Image.AdvertisedDigests {
		pinned[d] = struct{}{}
	}
	t := time.NewTicker(ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.evictImagesOnce(ctx, ttl, pinned)
		}
	}
}

func (w *Worker) evictImagesOnce(ctx context.Context, ttl time.Duration, pinned map[string]struct{}) {
	cutoff := time.Now().Add(-ttl).UnixNano()
	w.Images.Range(func(k, v any) bool {
		digest := k.(string)
		if _, ok := pinned[digest]; ok {
			return true
		}
		entry := v.(*imageEntry)
		if entry.lastUsed.Load() > cutoff {
			return true
		}
		// Drop the catalogue entry first so any concurrent ensureImage
		// for this digest takes the miss path and re-pulls cleanly.
		// CompareAndDelete loses harmlessly if the entry was replaced
		// (we'll catch it next tick). A racing touch between our Load
		// and Delete just costs one wasted re-pull — acceptable at this
		// cadence.
		if !w.Images.CompareAndDelete(digest, entry) {
			return true
		}
		if w.ImageStore == nil {
			return true
		}
		untagCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := w.ImageStore.UntagImage(untagCtx, digest); err != nil {
			log.Printf("worker: untag idle image %s: %v", digest, err)
		}
		cancel()
		return true
	})
}

func (w *Worker) runtimeType() gen.RuntimeType {
	// Map the configured runtime handler to the protocol enum so the
	// scheduler can match clients to compatible workers.
	switch w.Config.Runtime.Handler {
	case runtime.HandlerFirecracker:
		return gen.RuntimeType_FIRECRACKER
	case "runhcs-wcow-hypervisor":
		return gen.RuntimeType_HYPERV
	case runtime.HandlerReallyReallyDangerous:
		return gen.RuntimeType_DANGEROUS
	default:
		// Unknown runtime — default to FIRECRACKER and let the
		// scheduler reject if it disagrees. Logged so misconfig
		// surfaces in operations.
		log.Printf("worker: unrecognized runtime handler %q, defaulting to FIRECRACKER",
			w.Config.Runtime.Handler)
		return gen.RuntimeType_FIRECRACKER
	}
}

// --- helpers -------------------------------------------------------------

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "worker-" + hex.EncodeToString(b[:]), nil
}

func loadCertFingerprint(certFile string) ([]byte, error) {
	if certFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %q", certFile)
	}
	sum := sha256.Sum256(block.Bytes)
	return sum[:], nil
}
