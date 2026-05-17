package explain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// DefaultMaxEntries caps the on-disk record count. Each record is a
// few KB; 10k records ≈ tens of MB worst case. Matches the
// "10k records" choice in docs/plan/phase-5-observability.md §5.3.
const DefaultMaxEntries = 10000

// Store is the explain-record persistence interface. Two methods so a
// future remote-explain (worker-side records joined into the daemon
// view) is a drop-in replacement.
type Store interface {
	// Get returns the latest record for sourcePath, or (nil, nil) if
	// none. Errors propagate only for I/O failures the daemon should
	// surface; "missing" is not an error.
	Get(sourcePath string) (*Record, error)

	// Put writes r. Triggers eviction when the on-disk record count
	// exceeds the store's cap.
	Put(r *Record) error
}

// DiskStore stores one JSON file per source path under Dir. Filenames
// are sha256(abspath(source))+".json" so paths with separators or
// odd characters don't collide and can't escape the dir.
//
// Concurrent-safe across goroutines in the same process via
// atomic rename; concurrent across processes is fine — last writer
// wins and a partially-written file is never observed because we
// write to a tempfile first.
type DiskStore struct {
	Dir        string
	MaxEntries int // 0 → DefaultMaxEntries
}

var _ Store = (*DiskStore)(nil)

// NewDiskStore returns a DiskStore writing under dir, creating the
// dir if needed. maxEntries == 0 picks the package default.
func NewDiskStore(dir string, maxEntries int) (*DiskStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("explain: mkdir %q: %w", dir, err)
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &DiskStore{Dir: dir, MaxEntries: maxEntries}, nil
}

// Get reads the record for sourcePath. Returns (nil, nil) when no
// record exists or the on-disk version doesn't match recordVersion —
// stale records from a downgraded binary are silently ignored rather
// than surfaced as garbage.
func (s *DiskStore) Get(sourcePath string) (*Record, error) {
	p := filepath.Join(s.Dir, HashSourcePath(sourcePath))
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("explain: read %q: %w", p, err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("explain: parse %q: %w", p, err)
	}
	if r.Version != recordVersion {
		return nil, nil
	}
	return &r, nil
}

// Put writes r atomically and runs LRU eviction when the record
// count exceeds MaxEntries. r.SourcePath must be set; the daemon
// builds the record with an abspath, so we don't re-resolve here.
func (s *DiskStore) Put(r *Record) error {
	if r == nil || r.SourcePath == "" {
		return errors.New("explain: Put: SourcePath is required")
	}
	if r.Version == 0 {
		r.Version = recordVersion
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = nowFunc()
	}
	final := filepath.Join(s.Dir, HashSourcePath(r.SourcePath))
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("explain: encode: %w", err)
	}
	// Atomic write: tempfile in the same dir + rename, so a reader
	// can't observe a partial JSON document.
	tmp, err := os.CreateTemp(s.Dir, "explain-*.tmp")
	if err != nil {
		return fmt.Errorf("explain: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("explain: write tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("explain: close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("explain: rename: %w", err)
	}
	cleanup = false

	if err := s.evictIfOver(); err != nil {
		// Eviction failure shouldn't fail the Put — the record we
		// just wrote is still valid, the dir just has more entries
		// than the operator asked for.
		return fmt.Errorf("explain: evict: %w", err)
	}
	return nil
}

// evictIfOver counts JSON record files under Dir and, if the count
// exceeds MaxEntries, deletes the oldest-mtime files until under the
// cap. Best-effort: a stat error on one file is logged-and-skipped
// (we just don't consider it for eviction).
func (s *DiskStore) evictIfOver() error {
	type entry struct {
		path  string
		mtime int64
	}
	var entries []entry
	walkErr := filepath.WalkDir(s.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path: path, mtime: info.ModTime().UnixNano()})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	excess := len(entries) - s.MaxEntries
	if excess <= 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime < entries[j].mtime
	})
	for i := 0; i < excess; i++ {
		_ = os.Remove(entries[i].path)
	}
	return nil
}

// nowFunc is the timestamp source. Indirected for tests.
var nowFunc = defaultNow

// DefaultDir returns the user's standard explain-store location:
// `<os.UserCacheDir>/hpcc/explain`. The directory is not created
// here — NewDiskStore mkdirs on first Put.
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "hpcc", "explain"), nil
}
