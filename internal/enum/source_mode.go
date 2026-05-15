package enum

import "fmt"

// SourceMode is the single knob that picks both:
//
//  1. How the local compile cache derives its key when no precomputed
//     digest is available (i.e. the client-side path; the worker
//     short-circuits via the digest the client sends).
//  2. How the client ships source bytes to the worker when remote
//     dispatch is enabled.
//
// Both behaviors track the same value so a client's local cache and
// a worker-fronted shared cache (S3, paranoid mode) stay coherent —
// without that pairing, the two sides would derive different keys
// for the same translation unit and never see each other's entries.
//
// "preprocessed" → SourceModePreprocessed:
//
//	Cache key: hash the bytes produced by the full preprocessor
//	(gcc -E / cl /E).
//	Dispatch (when remote): client preprocesses and ships the
//	resulting bytes inline in CompileRequest. Works on every
//	workload that produces a self-contained translation unit.
//
// "cas" → SourceModeCAS:
//
//	Cache key: hash the dep closure (gcc -M / cl /showIncludes).
//	No macro expansion, no preprocessed-text output — much cheaper
//	than the preprocessed-bytes hash.
//	Dispatch (when remote): client builds a content-addressed
//	manifest, probes the worker's compile cache, and (on miss)
//	ships only the missing source blobs via UploadBlobs. Required
//	for workloads where preprocessed output isn't self-contained
//	(e.g. GAS .S files with .incbin). See docs/cas.md.
//
// Maps 1:1 to protocol.SourceMode but lives in `enum` so the config
// package can hold it without dragging the generated proto into its
// import graph.
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
