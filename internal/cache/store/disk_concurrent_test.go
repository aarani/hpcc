package store

import (
	"bytes"
	"sync"
	"testing"
)

// Two concurrent Put calls for the same key/name on freshly-created
// Namespace wrappers must both succeed. Before the singleflight guard
// in DiskCacheStore.Put, this raced on a shared `<dst>.tmp` path: the
// first Rename consumed the tmp, the second hit ENOENT. The worker's
// UploadBlobs handler builds a fresh Namespace per call (see
// internal/worker/cas.go), so the dedup must travel through Namespace.
func TestDiskConcurrentPut_sameKey(t *testing.T) {
	parent, err := NewDiskCacheStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewDiskCacheStore: %v", err)
	}

	const n = 32
	key := []byte("abcdef0123456789")
	value := bytes.Repeat([]byte("x"), 64*1024)

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ns := parent.Namespace("tenant")
			errs[i] = ns.Put(key, "data", value)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Put #%d failed: %v", i, err)
		}
	}

	got, err := parent.Namespace("tenant").Get(key, "data")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("Get returned %d bytes, want %d", len(got), len(value))
	}
}
