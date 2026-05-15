package store

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aarani/hpcc/internal/config"
)

// DiskCacheStore is a content-addressable on-disk store. Each key maps
// to a directory `<dir>/<hh>/<full-hex-key>/` that holds one file per
// named blob (output, stdout, stderr, exit_code, metadata, ...).
type DiskCacheStore struct {
	dir     string
	maxSize int64 // bytes; 0 means unlimited
}

func NewDiskCacheStore(dir string, maxSize string) (*DiskCacheStore, error) {
	sz, err := config.ParseSize(maxSize)
	if err != nil {
		return nil, fmt.Errorf("disk cache max_size: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %q: %w", dir, err)
	}
	return &DiskCacheStore{dir: dir, maxSize: sz}, nil
}

func (d *DiskCacheStore) Get(key []byte, name string) ([]byte, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	p := filepath.Join(d.entryDir(key), name)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cache entry: %w", err)
	}
	return data, nil
}

func (d *DiskCacheStore) Put(key []byte, name string, value []byte) error {
	if err := validateName(name); err != nil {
		return err
	}
	dir := d.entryDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache subdir: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, name), value); err != nil {
		return err
	}
	if d.maxSize > 0 {
		return d.evict()
	}
	return nil
}

func (d *DiskCacheStore) Has(key []byte) (bool, error) {
	info, err := os.Stat(d.entryDir(key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat cache entry: %w", err)
	}
	return info.IsDir(), nil
}

// Dir returns the root directory of this cache store.
func (d *DiskCacheStore) Dir() string { return d.dir }

// Namespace returns a DiskCacheStore rooted at <d.dir>/<prefix>.
// maxSize is *not* inherited — namespaced stores default to unlimited
// so the caller decides whether to budget the namespace separately;
// otherwise three namespaces sharing one root would each evict to the
// parent's maxSize and overshoot 3x.
func (d *DiskCacheStore) Namespace(prefix string) Store {
	if err := validateNamespace(prefix); err != nil {
		panic(err)
	}
	sub := filepath.Join(d.dir, prefix)
	// Lazy MkdirAll on first Put avoids creating empty namespace
	// dirs that the caller may never use.
	return &DiskCacheStore{dir: sub, maxSize: 0}
}

// validateNamespace rejects prefixes that would escape the parent
// directory or stray outside a single path component.
func validateNamespace(prefix string) error {
	if prefix == "" {
		return errors.New("namespace prefix must not be empty")
	}
	if prefix == "." || prefix == ".." {
		return fmt.Errorf("invalid namespace prefix %q", prefix)
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if c == '/' || c == '\\' || c == 0 {
			return fmt.Errorf("invalid namespace prefix %q", prefix)
		}
	}
	return nil
}

// Stats returns the number of cache entries and their total size in bytes.
func (d *DiskCacheStore) Stats() (entries int, totalSize int64, err error) {
	ents, total, err := d.scanEntries()
	return len(ents), total, err
}

// Clean evicts cache entries to satisfy the given constraints. If maxSize
// is positive, LRU entries are removed until total size is at or below it.
// If maxAge is positive, entries whose newest blob is older than maxAge are
// removed regardless of size. Both constraints can be applied in one call.
func (d *DiskCacheStore) Clean(maxSize int64, maxAge time.Duration) error {
	entries, totalSize, err := d.scanEntries()
	if err != nil {
		return err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime < entries[j].modTime
	})

	cutoff := int64(0)
	if maxAge > 0 {
		cutoff = time.Now().Add(-maxAge).UnixNano()
	}

	for _, e := range entries {
		byAge := cutoff > 0 && e.modTime < cutoff
		bySize := maxSize > 0 && totalSize > maxSize
		if !byAge && !bySize {
			break
		}
		if err := os.RemoveAll(e.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		totalSize -= e.size
	}
	return nil
}

// entryDir returns the directory that holds all named blobs for the
// given key: <dir>/<first 2 hex chars>/<full hex key>/.
func (d *DiskCacheStore) entryDir(key []byte) string {
	h := hex.EncodeToString(key)
	if len(h) >= 2 {
		return filepath.Join(d.dir, h[:2], h)
	}
	return filepath.Join(d.dir, h)
}

// validateName rejects names that would escape the entry directory or
// collide with the atomic-write tempfile suffix.
func validateName(name string) error {
	if name == "" {
		return errors.New("cache blob name must not be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid cache blob name %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c == '\\' || c == 0 {
			return fmt.Errorf("invalid cache blob name %q", name)
		}
	}
	return nil
}

func writeAtomic(dst string, data []byte) error {
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp cache file: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename cache file: %w", err)
	}
	return nil
}

// entryInfo describes a single cache entry (a key directory) for the
// purposes of eviction. size is the sum of all blob sizes under it,
// and modTime is the most recent modification across those blobs so
// that touching any blob keeps the whole entry "fresh".
type entryInfo struct {
	path    string
	size    int64
	modTime int64
}

func (d *DiskCacheStore) evict() error {
	entries, totalSize, err := d.scanEntries()
	if err != nil {
		return err
	}
	if totalSize <= d.maxSize {
		return nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime < entries[j].modTime
	})

	for _, e := range entries {
		if totalSize <= d.maxSize {
			break
		}
		if err := os.RemoveAll(e.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		totalSize -= e.size
	}
	return nil
}

// scanEntries walks the two-level layout (<hh>/<full>/...) and reports
// one entry per <full> directory, summing the sizes of its contents.
func (d *DiskCacheStore) scanEntries() ([]entryInfo, int64, error) {
	var entries []entryInfo
	var total int64

	shards, err := os.ReadDir(d.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read cache dir: %w", err)
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(d.dir, shard.Name())
		keys, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if !k.IsDir() {
				continue
			}
			keyPath := filepath.Join(shardPath, k.Name())
			size, mod, err := dirStats(keyPath)
			if err != nil {
				continue
			}
			entries = append(entries, entryInfo{
				path:    keyPath,
				size:    size,
				modTime: mod,
			})
			total += size
		}
	}
	return entries, total, nil
}

// dirStats returns the total size and most-recent mtime of the regular
// files directly inside dir.
func dirStats(dir string) (int64, int64, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	var size, mod int64
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		size += info.Size()
		if t := info.ModTime().UnixNano(); t > mod {
			mod = t
		}
	}
	return size, mod, nil
}
