package explain

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestCompareToNamesChangedFields(t *testing.T) {
	prior := &Record{
		CompilerIdentityHash: "aaa",
		FlagsHash:            "bbb",
		SourceContentHash:    "ccc",
		HeaderHashes: map[string]string{
			"foo.h": "111",
			"bar.h": "222",
		},
		ImageDigest: "img1",
	}
	now := &Record{
		CompilerIdentityHash: "aaa",
		FlagsHash:            "BBB", // changed
		SourceContentHash:    "ccc",
		HeaderHashes: map[string]string{
			"foo.h": "111", // unchanged
			"bar.h": "999", // changed
			"baz.h": "777", // added
		},
		ImageDigest: "img1",
	}
	diffs := now.CompareTo(prior)

	// Expect: flags, header(bar.h changed), header(baz.h added)
	if len(diffs) != 3 {
		t.Fatalf("expected 3 diffs, got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].Kind != DiffFlags {
		t.Fatalf("expected flags diff first, got %+v", diffs[0])
	}
	// Headers are sorted: bar.h then baz.h.
	if diffs[1].Kind != DiffHeader || diffs[1].Path != "bar.h" || diffs[1].Before != "222" || diffs[1].After != "999" {
		t.Fatalf("unexpected header diff for bar.h: %+v", diffs[1])
	}
	if diffs[2].Kind != DiffHeader || diffs[2].Path != "baz.h" || diffs[2].Before != "" || diffs[2].After != "777" {
		t.Fatalf("unexpected header diff for baz.h: %+v", diffs[2])
	}
}

func TestCompareToNilPriorReturnsNil(t *testing.T) {
	r := &Record{FlagsHash: "x"}
	if diffs := r.CompareTo(nil); diffs != nil {
		t.Fatalf("expected nil diffs for nil prior, got %+v", diffs)
	}
}

func TestDiskStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(dir, "foo.c")
	r := &Record{
		SourcePath:        src,
		OutputPath:        filepath.Join(dir, "foo.o"),
		Outcome:           OutcomeLocalInvoke,
		CacheKey:          "deadbeef",
		FlagsHash:         "ff",
		SourceContentHash: "ee",
	}
	if err := s.Put(r); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(src)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil after Put")
	}
	if got.Outcome != OutcomeLocalInvoke || got.CacheKey != "deadbeef" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Version != recordVersion {
		t.Fatalf("expected version %d, got %d", recordVersion, got.Version)
	}
	if got.Timestamp.IsZero() {
		t.Fatal("expected Put to backfill Timestamp")
	}
}

func TestDiskStoreGetMissingIsNilNil(t *testing.T) {
	s, err := NewDiskStore(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Get("/nope/missing.c")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if r != nil {
		t.Fatalf("expected nil record, got %+v", r)
	}
}

func TestDiskStoreEvictsOverCap(t *testing.T) {
	// Drive nowFunc to give each record a deterministic, increasing
	// timestamp; the eviction order is by file mtime which on most
	// filesystems tracks now() at write time, but tests on coarse-
	// resolution clocks (HFS+) flake when records land in the same
	// tick. Fix the times explicitly.
	defer func(orig func() time.Time) { nowFunc = orig }(nowFunc)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	nowFunc = func() time.Time {
		out := t0.Add(time.Duration(i) * time.Second)
		i++
		return out
	}

	dir := t.TempDir()
	s, err := NewDiskStore(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	srcs := []string{"/a.c", "/b.c", "/c.c", "/d.c", "/e.c"}
	for n, src := range srcs {
		r := &Record{SourcePath: src, Outcome: OutcomeLocalInvoke}
		if err := s.Put(r); err != nil {
			t.Fatalf("Put %d: %v", n, err)
		}
		// Bump the file's mtime to match the recorded ts so the
		// eviction walker observes the same ordering as the writes.
		ts := t0.Add(time.Duration(n) * time.Second)
		_ = os.Chtimes(filepath.Join(dir, HashSourcePath(src)), ts, ts)
	}

	// After 5 inserts with cap=3 the oldest two (a.c, b.c) should
	// be evicted. The eviction itself ran on the 5th Put; re-run
	// evict to catch the case where eviction needed a second pass
	// (it doesn't, but a stale assertion would catch it).
	if err := s.evictIfOver(); err != nil {
		t.Fatal(err)
	}

	var remaining []string
	for _, src := range srcs {
		got, err := s.Get(src)
		if err != nil {
			t.Fatalf("Get %s: %v", src, err)
		}
		if got != nil {
			remaining = append(remaining, src)
		}
	}
	sort.Strings(remaining)
	want := []string{"/c.c", "/d.c", "/e.c"}
	if !reflect.DeepEqual(remaining, want) {
		t.Fatalf("eviction wrong: kept %v, want %v", remaining, want)
	}
}
