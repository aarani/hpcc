package cache

import (
	"testing"

	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
)

// Store must not persist a compile that exited non-zero. The previous
// behaviour wrote stdout/stderr/exit_code/metadata regardless of exit
// code, so one transient remote failure (OOM kill, missing toolchain
// package on the worker image, vsock disconnect mid-compile) poisoned
// the TU's cache slot and every subsequent build replayed the
// failure until the cache was manually cleaned.
func TestCompileCacheStore_skipsFailedCompiles(t *testing.T) {
	ds, err := store.NewDiskCacheStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewDiskCacheStore: %v", err)
	}

	// nil Context is fine — Store with non-zero exit returns before
	// any field on it is touched.
	cc := NewCompileCache(nil, []store.Store{ds})

	inv := &compiler.Invocation{}
	res := &compiler.InvocationResult{
		ExitCode: 1,
		Stderr:   []byte("fatal error: 'gelf.h' file not found\n"),
	}
	if err := cc.Store(inv, res, "tenant-a"); err != nil {
		t.Fatalf("Store returned err on non-zero exit: %v", err)
	}

	n, sz, err := ds.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if n != 0 || sz != 0 {
		t.Errorf("expected no cache entries after failed-compile Store, got %d entries / %d bytes", n, sz)
	}
}
