package store

import "time"

// Store is a content-addressable store keyed by an opaque (typically
// hash) byte key. Each key maps to a small set of named blobs, so a
// single cache entry can hold the compiler artifact alongside its
// stdout, stderr, exit code, and metadata.
//
// Conventional names used by the compiler cache:
//
//	"output"      - the compiler output object (e.g. .o)
//	"stdout"      - captured stdout
//	"stderr"      - captured stderr
//	"exit_code"   - exit code as ASCII
//	"metadata"    - JSON metadata (timestamp, compiler, command, ...)
type Store interface {
	// Get returns the value stored under (key, name), or nil if no
	// such entry exists. A missing entry is not an error.
	Get(key []byte, name string) ([]byte, error)

	// Put stores value under (key, name). If the cache has a size
	// limit, it should evict entries as needed to stay within it.
	// Eviction is performed at the key (entry) granularity: all
	// named blobs sharing a key are evicted together.
	Put(key []byte, name string, value []byte) error

	// Has reports whether any blob exists for the given key.
	Has(key []byte) (bool, error)

	Stats() (entries int, totalSize int64, err error)

	Clean(maxSize int64, maxAge time.Duration) error

	Dir() string
}
