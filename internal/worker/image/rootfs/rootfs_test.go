package rootfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestEncodeDecodeRootfsName_roundtrip(t *testing.T) {
	cases := []struct {
		digest string
		file   string
	}{
		{"sha256:abc123", "sha256-abc123.ext4"},
		{"sha512:deadbeef", "sha512-deadbeef.ext4"},
		// Bare hex assumed sha256 (matches cdimage.normalizeDigest).
		{"abc", "sha256-abc.ext4"},
	}
	for _, c := range cases {
		got, err := encodeRootfsName(c.digest)
		if err != nil {
			t.Fatalf("encodeRootfsName(%q): %v", c.digest, err)
		}
		if got != c.file {
			t.Errorf("encodeRootfsName(%q) = %q, want %q", c.digest, got, c.file)
		}

		// Decoding always normalizes back to "<algo>:<hex>".
		want := c.digest
		if !contains(want, ":") {
			want = "sha256:" + want
		}
		dec, ok := decodeRootfsName(c.file)
		if !ok {
			t.Errorf("decodeRootfsName(%q) returned ok=false", c.file)
			continue
		}
		if dec != want {
			t.Errorf("decodeRootfsName(%q) = %q, want %q", c.file, dec, want)
		}
	}
}

func TestEncodeRootfsName_rejectsInvalid(t *testing.T) {
	for _, in := range []string{"", ":abc", "sha256:"} {
		if _, err := encodeRootfsName(in); err == nil {
			t.Errorf("encodeRootfsName(%q) succeeded, want error", in)
		}
	}
}

func TestDecodeRootfsName_rejectsStrays(t *testing.T) {
	for _, name := range []string{
		"random.txt",          // wrong suffix
		"sha256.ext4",         // no dash
		"-abc.ext4",           // empty algo
		"sha256-.ext4",        // empty hex
		".ext4",               // empty stem
		"sha256-abc.ext4.bak", // wrong suffix
	} {
		if _, ok := decodeRootfsName(name); ok {
			t.Errorf("decodeRootfsName(%q) returned ok=true, want false", name)
		}
	}
}

func TestGetExistingImages_listsAndFiltersStrays(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "sha256-aaaa.ext4"), nil)
	mustWrite(t, filepath.Join(dir, "sha256-bbbb.ext4"), nil)
	// Strays the listing must skip silently.
	mustWrite(t, filepath.Join(dir, "in-progress.tmp"), nil)
	mustWrite(t, filepath.Join(dir, "README"), nil)
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Store{CacheDir: dir}
	got, err := s.GetExistingImages(context.Background())
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}
	sort.Strings(got)
	want := []string{"sha256:aaaa", "sha256:bbbb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGetExistingImages_missingCacheDirIsEmpty(t *testing.T) {
	s := &Store{CacheDir: filepath.Join(t.TempDir(), "does-not-exist")}
	got, err := s.GetExistingImages(context.Background())
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestUntagImage_removesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sha256-cafe.ext4")
	mustWrite(t, path, []byte("rootfs"))

	s := &Store{CacheDir: dir}
	if err := s.UntagImage(context.Background(), "sha256:cafe"); err != nil {
		t.Fatalf("UntagImage: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rootfs file still present after UntagImage: %v", err)
	}

	// Second call must succeed — eviction can race against catalogue
	// drift and a crash that left no artifact.
	if err := s.UntagImage(context.Background(), "sha256:cafe"); err != nil {
		t.Errorf("UntagImage idempotent call returned %v", err)
	}
}

func TestUntagImage_rejectsInvalidDigest(t *testing.T) {
	s := &Store{CacheDir: t.TempDir()}
	if err := s.UntagImage(context.Background(), ""); err == nil {
		t.Errorf("UntagImage(\"\") succeeded, want error")
	}
}

func TestExt4SizeFor_bounds(t *testing.T) {
	// Tiny content still gets the minimum and stays 4MiB-aligned.
	got := ext4SizeFor(1024)
	if got != ext4MinBytes {
		t.Errorf("ext4SizeFor(1024) = %d, want %d (min)", got, ext4MinBytes)
	}
	if got%ext4Mib4 != 0 {
		t.Errorf("ext4SizeFor(1024) = %d, not 4MiB-aligned", got)
	}
	// Larger content scales past the floor and stays aligned.
	huge := int64(500 * 1024 * 1024)
	got = ext4SizeFor(huge)
	if got <= huge*ext4ContentScale {
		t.Errorf("ext4SizeFor(%d) = %d, want > content*scale", huge, got)
	}
	if got%ext4Mib4 != 0 {
		t.Errorf("ext4SizeFor(%d) = %d, not 4MiB-aligned", huge, got)
	}
}

// TestBuildExt4FromDir_endToEnd skips on hosts where mkfs.ext4 isn't
// available (Mac dev workstations, container images without
// e2fsprogs). When the binary is present, it stages a tiny dir,
// builds an ext4 from it, and asserts the magic bytes at offset
// 1080 — the ext4 superblock magic 0xEF53 — to confirm we got a
// real filesystem.
func TestBuildExt4FromDir_endToEnd(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skipf("mkfs.ext4 not available: %v", err)
	}

	staging := t.TempDir()
	mustWrite(t, filepath.Join(staging, "hostname"), []byte("hpcc-vm\n"))
	if err := os.MkdirAll(filepath.Join(staging, "usr", "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(staging, "usr", "bin", "cc"), []byte("fake-cc"))

	out := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := buildExt4FromDir(staging, out); err != nil {
		t.Fatalf("buildExt4FromDir: %v", err)
	}

	// ext4 superblock starts at offset 1024; magic is at offset 56
	// inside the superblock (1024 + 56 = 1080), little-endian
	// 0xEF53. Anything else means mkfs.ext4 didn't actually format.
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open rootfs: %v", err)
	}
	defer f.Close()
	var sb [2]byte
	if _, err := f.ReadAt(sb[:], 1080); err != nil {
		t.Fatalf("read superblock magic: %v", err)
	}
	if sb[0] != 0x53 || sb[1] != 0xEF {
		t.Errorf("ext4 magic at offset 1080 = % x, want 53 ef", sb[:])
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// Must hit the minimum floor for a tiny staging dir.
	if info.Size() < ext4MinBytes {
		t.Errorf("rootfs size = %d, want >= %d", info.Size(), ext4MinBytes)
	}
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
