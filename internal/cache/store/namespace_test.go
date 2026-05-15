package store

import (
	"bytes"
	"testing"
)

func TestDiskNamespace_isolatesKeys(t *testing.T) {
	parent, err := NewDiskCacheStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewDiskCacheStore: %v", err)
	}

	a := parent.Namespace("compile")
	b := parent.Namespace("source")

	key := []byte("abcdef0123456789")
	if err := a.Put(key, "output", []byte("from compile")); err != nil {
		t.Fatalf("a.Put: %v", err)
	}
	if err := b.Put(key, "output", []byte("from source")); err != nil {
		t.Fatalf("b.Put: %v", err)
	}

	got, err := a.Get(key, "output")
	if err != nil {
		t.Fatalf("a.Get: %v", err)
	}
	if !bytes.Equal(got, []byte("from compile")) {
		t.Errorf("a.Get returned %q, want %q", got, "from compile")
	}

	got, err = b.Get(key, "output")
	if err != nil {
		t.Fatalf("b.Get: %v", err)
	}
	if !bytes.Equal(got, []byte("from source")) {
		t.Errorf("b.Get returned %q, want %q", got, "from source")
	}

	// Parent shouldn't see either entry — its scan is rooted at the
	// parent dir and doesn't descend into namespace subdirs.
	parentHas, err := parent.Has(key)
	if err != nil {
		t.Fatalf("parent.Has: %v", err)
	}
	if parentHas {
		t.Errorf("parent.Has returned true for a namespaced key; namespaces should be invisible to the parent")
	}
}

func TestDiskNamespace_rejectsBadPrefix(t *testing.T) {
	parent, err := NewDiskCacheStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewDiskCacheStore: %v", err)
	}

	for _, bad := range []string{"", ".", "..", "a/b", "a\\b", "a\x00b"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Namespace(%q) should have panicked", bad)
				}
			}()
			_ = parent.Namespace(bad)
		})
	}
}
