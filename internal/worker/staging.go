package worker

import (
	"fmt"
	"os"
	"path/filepath"

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
//
// CAS mode (§4.5) was scoped but deferred; the proto reserves the wire
// tags. Requests carrying SourceMode = 1 hit the default branch below
// and are rejected as unknown.
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
	default:
		cleanup()
		return "", "", nil, fmt.Errorf("unknown source_mode %v", req.Descriptor_.SourceMode)
	}

	return srcDir, outDir, cleanup, nil
}
