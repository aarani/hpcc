package enum

import "fmt"

// SourceMode selects how the client ships source bytes to the worker.
// Maps 1:1 to protocol.SourceMode but lives in `enum` so the config
// package can hold it without dragging the generated proto into its
// import graph.
//
// "preprocessed" → SourceModePreprocessed: client preprocesses,
//
//	ships the resulting bytes inline in CompileRequest. Compatible
//	with every workload that produces a self-contained translation
//	unit; the default for v1.
//
// "cas" → SourceModeCAS: client builds a content-addressed manifest,
//
//	probes the worker's compile cache, and (on miss) ships only the
//	missing source blobs via UploadBlobs before compiling. Wins
//	cross-worker and cross-developer compile-cache hits; required
//	for workloads where preprocessed output isn't self-contained
//	(e.g. GAS .S files with .incbin). See docs/cas.md.
type SourceMode int

const (
	SourceModeUnspecified SourceMode = iota
	SourceModePreprocessed
	SourceModeCAS
)

func (m SourceMode) MarshalText() ([]byte, error) {
	switch m {
	case SourceModePreprocessed:
		return []byte("preprocessed"), nil
	case SourceModeCAS:
		return []byte("cas"), nil
	case SourceModeUnspecified:
		return []byte(""), nil
	}
	return nil, fmt.Errorf("invalid source mode %d", m)
}

func (m *SourceMode) UnmarshalText(text []byte) error {
	switch string(text) {
	case "preprocessed":
		*m = SourceModePreprocessed
	case "cas":
		*m = SourceModeCAS
	case "":
		*m = SourceModeUnspecified
	default:
		return fmt.Errorf("unknown source mode %q (want preprocessed|cas)", text)
	}
	return nil
}
