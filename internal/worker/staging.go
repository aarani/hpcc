package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/protocol/gen"
)

// preprocessedSourceName is the in-container filename the worker writes
// PreprocessedDescriptor bytes to. The client must have rewritten its
// argv to reference "/src/<this>" before sending. Hardcoded for v1 so
// the descriptor doesn't need a separate "filename" field; revisit if
// multiple inputs per request is ever a thing.
const preprocessedSourceName = "main.i"

// stageSource materializes the source side of a Compile RPC onto the
// worker's local filesystem according to the descriptor's source_mode.
// Returns the host-side paths that should be plumbed into ContainerSpec
// (which the runtime then exposes inside the sandbox at /src and /out)
// plus a cleanup func the caller must defer.
//
// Each source mode has its own staging contract:
//
//   - PREPROCESSED: write the inline preprocessed bytes to
//     <srcDir>/main.i; allocate an empty <outDir> for artifacts.
//   - CAS (docs/cas.md): materialize every project-relative blob from
//     the CasDescriptor onto srcDir under its declared path, by
//     looking up the bytes in the worker-local source store. Absolute
//     (system) paths are skipped — those files live in the toolchain
//     image, not in the source closure we ferry.
//
// Anything else hits the default branch below and is rejected as
// unknown.
func (w *Worker) stageSource(req *gen.CompileRequest) (srcHostPath, outHostPath string, cleanup func(), err error) {
	srcDir, err := os.MkdirTemp("", "hpcc-src-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("alloc src tmpdir: %w", err)
	}
	outDir, err := os.MkdirTemp("", "hpcc-out-*")
	if err != nil {
		_ = os.RemoveAll(srcDir)
		return "", "", nil, fmt.Errorf("alloc out tmpdir: %w", err)
	}
	cleanup = func() {
		_ = os.RemoveAll(srcDir)
		_ = os.RemoveAll(outDir)
	}

	switch req.Descriptor_.SourceMode {
	case gen.SourceMode_PREPROCESSED:
		pp := req.Descriptor_.GetPreprocessed()
		if pp == nil {
			cleanup()
			return "", "", nil, fmt.Errorf("source_mode=PREPROCESSED but preprocessed_settings is empty")
		}
		path := filepath.Join(srcDir, preprocessedSourceName)
		if err := os.WriteFile(path, pp.PreprocessedSource, 0o600); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("write preprocessed source: %w", err)
		}
	case gen.SourceMode_CAS:
		cas := req.Descriptor_.GetCas()
		if cas == nil {
			cleanup()
			return "", "", nil, fmt.Errorf("source_mode=CAS but cas descriptor is empty")
		}
		if w.sourceStore == nil {
			cleanup()
			return "", "", nil, fmt.Errorf("source_mode=CAS but worker has no source store configured")
		}
		if err := materializeCASBlobs(srcDir, cas, w.sourceStore); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("materialize CAS blobs: %w", err)
		}
	default:
		cleanup()
		return "", "", nil, fmt.Errorf("unknown source_mode %v", req.Descriptor_.SourceMode)
	}

	return srcDir, outDir, cleanup, nil
}

// materializeCASBlobs writes every project-relative blob in cas.Blobs
// into srcDir under its declared path. Absolute paths are skipped —
// they're system paths that already exist inside the toolchain image
// (e.g. /usr/include/...) and don't need to be staged. Each path is
// re-validated to refuse traversal (..), absolute slashes, NUL bytes,
// and Windows-style backslashes before it touches the filesystem.
//
// Missing blobs (sourceStore.Get returns nil) fail the staging — the
// client should have uploaded everything missing in the dance before
// calling Compile. A miss here means a protocol bug or a race with
// eviction; either way the right thing to do is fail loud and let the
// client retry from the probe step.
func materializeCASBlobs(srcDir string, cas *gen.CasDescriptor, src store.Store) error {
	for _, b := range cas.Blobs {
		if filepath.IsAbs(b.Path) {
			// System path; the file is in the image, not in the
			// source closure we materialize.
			continue
		}
		if err := validateBlobPath(b.Path); err != nil {
			return fmt.Errorf("blob path %q: %w", b.Path, err)
		}
		bytes, err := src.Get(b.Digest, blobData)
		if err != nil {
			return fmt.Errorf("load blob %x: %w", b.Digest, err)
		}
		if bytes == nil {
			return fmt.Errorf("blob %x unavailable: client must re-upload", b.Digest)
		}
		dst := filepath.Join(srcDir, filepath.FromSlash(b.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create parent for %q: %w", b.Path, err)
		}
		if err := os.WriteFile(dst, bytes, 0o600); err != nil {
			return fmt.Errorf("write %q: %w", b.Path, err)
		}
	}
	return nil
}

// validateBlobPath rejects paths that would escape srcDir on
// materialization or otherwise misbehave. Run on every BlobRef.Path
// before joining onto srcDir — the path is client-supplied data,
// even though the worker only acts on it after the manifest digest
// has been verified to match the (path, content) pairs the client
// claims.
func validateBlobPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path contains NUL")
	}
	// Reject Windows-style separators; we treat paths as forward-slash
	// everywhere on the wire and on disk under srcDir.
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("path contains backslash")
	}
	// filepath.Clean collapses .. and . segments. If cleaning produces
	// a path that starts with .. or is absolute, the original tried to
	// escape srcDir.
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path escapes source root")
	}
	if strings.HasPrefix(clean, "/") {
		// Caller is responsible for filtering absolute paths upstream;
		// guard here too in case the contract drifts.
		return fmt.Errorf("absolute path not allowed")
	}
	return nil
}
