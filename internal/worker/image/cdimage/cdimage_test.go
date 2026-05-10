package cdimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// --- pure helpers --------------------------------------------------------

func TestNormalizeDigest(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"sha256:abc", "sha256:abc"},
		{"abc", "sha256:abc"},
		{"sha512:def", "sha512:def"}, // any algo prefix is preserved
		{"", "sha256:"},              // empty hash → still gets prefix; not graceful but predictable
	}
	for _, c := range cases {
		got := normalizeDigest(c.in)
		if string(got) != c.want {
			t.Errorf("normalizeDigest(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPreparedImageName(t *testing.T) {
	d := digest.Digest("sha256:abc123")
	got := preparedImageName(d)
	want := "prepared.hpcc.local/img:abc123"
	if got != want {
		t.Errorf("preparedImageName(%q) = %q, want %q", d, got, want)
	}
}

func TestIsManifestMediaType(t *testing.T) {
	yes := []string{
		ocispec.MediaTypeImageManifest,
		"application/vnd.docker.distribution.manifest.v2+json",
	}
	no := []string{
		"",
		ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.layer.v1.tar+gzip",
		"application/json",
	}
	for _, mt := range yes {
		if !isManifestMediaType(mt) {
			t.Errorf("isManifestMediaType(%q) = false, want true", mt)
		}
	}
	for _, mt := range no {
		if isManifestMediaType(mt) {
			t.Errorf("isManifestMediaType(%q) = true, want false", mt)
		}
	}
}

func TestParentDirs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{".hpcc/pause", []string{".hpcc"}},
		{"a/b/c/d", []string{"a", "a/b", "a/b/c"}},
		{"single", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := parentDirs(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parentDirs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNormalizeLayerPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/.hpcc/pause", ".hpcc/pause"},
		{".hpcc/pause", ".hpcc/pause"},
		{`C:\.hpcc\pause.exe`, "Files/.hpcc/pause.exe"},
		{`\.hpcc\pause.exe`, "Files/.hpcc/pause.exe"},
		{"Files/.hpcc/pause.exe", "Files/.hpcc/pause.exe"},
	}
	for _, c := range cases {
		got := normalizeLayerPath(c.in)
		if got != c.want {
			t.Errorf("normalizeLayerPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- buildPauseLayer -----------------------------------------------------

func TestBuildPauseLayer_linuxTarStructure(t *testing.T) {
	pause := []byte("fake-pause-binary")
	uncompressed, compressed, err := buildPauseLayer("/.hpcc/pause", pause)
	if err != nil {
		t.Fatalf("buildPauseLayer: %v", err)
	}
	if len(uncompressed) == 0 || len(compressed) == 0 {
		t.Fatalf("empty layer outputs: u=%d c=%d", len(uncompressed), len(compressed))
	}

	entries := readTar(t, uncompressed)
	want := map[string]struct {
		typeflag byte
		mode     int64
		body     []byte
	}{
		".hpcc/":      {tar.TypeDir, 0o755, nil},
		".hpcc/pause": {tar.TypeReg, 0o755, pause},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(want), entries)
	}
	for name, w := range want {
		got, ok := entries[name]
		if !ok {
			t.Errorf("missing entry %q", name)
			continue
		}
		if got.hdr.Typeflag != w.typeflag {
			t.Errorf("entry %q typeflag = %d, want %d", name, got.hdr.Typeflag, w.typeflag)
		}
		if got.hdr.Mode != w.mode {
			t.Errorf("entry %q mode = %o, want %o", name, got.hdr.Mode, w.mode)
		}
		if !bytes.Equal(got.body, w.body) {
			t.Errorf("entry %q body = %q, want %q", name, got.body, w.body)
		}
	}
}

func TestBuildPauseLayer_windowsTarStructure(t *testing.T) {
	pause := []byte("MZfake")
	uncompressed, _, err := buildPauseLayer(`C:\.hpcc\pause.exe`, pause)
	if err != nil {
		t.Fatalf("buildPauseLayer: %v", err)
	}
	entries := readTar(t, uncompressed)
	mustHave := []string{"Files", "Files/.hpcc", "Files/.hpcc/pause.exe"}
	for _, name := range mustHave {
		if _, ok := entries[name]; !ok && !contains(entries, name+"/") {
			t.Errorf("missing entry %q", name)
		}
	}
	body := entries["Files/.hpcc/pause.exe"].body
	if !bytes.Equal(body, pause) {
		t.Errorf("pause body mismatch: %q vs %q", body, pause)
	}
}

func TestBuildPauseLayer_compressedDecompressesToUncompressed(t *testing.T) {
	uncompressed, compressed, err := buildPauseLayer("/.hpcc/pause", []byte("hello"))
	if err != nil {
		t.Fatalf("buildPauseLayer: %v", err)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if !bytes.Equal(got, uncompressed) {
		t.Fatalf("gzip(uncompressed) round-trip mismatch")
	}
}

func TestBuildPauseLayer_deterministic(t *testing.T) {
	// Same inputs must produce identical bytes — the layer descriptor
	// digest is what flows into the manifest, and a flapping digest
	// would defeat the prepared-image cache on the worker.
	a, _, err := buildPauseLayer("/.hpcc/pause", []byte("xyz"))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := buildPauseLayer("/.hpcc/pause", []byte("xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("buildPauseLayer is non-deterministic")
	}
}

func TestBuildPauseLayer_emptyBinary(t *testing.T) {
	uncompressed, _, err := buildPauseLayer("/.hpcc/pause", nil)
	if err != nil {
		t.Fatalf("buildPauseLayer(nil): %v", err)
	}
	entries := readTar(t, uncompressed)
	body := entries[".hpcc/pause"].body
	if len(body) != 0 {
		t.Errorf("empty binary should yield zero-byte file entry, got %d bytes", len(body))
	}
}

// --- loadPauseBinary -----------------------------------------------------

func TestLoadPauseBinary_linuxAmd64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pause-linux-amd64")
	if err := os.WriteFile(path, []byte("L64"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Store{Pause: PauseBinaries{LinuxAmd64: path}}

	bin, install, err := s.loadPauseBinary("linux", "amd64")
	if err != nil {
		t.Fatalf("loadPauseBinary: %v", err)
	}
	if string(bin) != "L64" {
		t.Errorf("bin = %q, want L64", bin)
	}
	if install != "/.hpcc/pause" {
		t.Errorf("install = %q, want /.hpcc/pause", install)
	}
}

func TestLoadPauseBinary_linuxArm64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pause-linux-arm64")
	if err := os.WriteFile(path, []byte("ARM"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Store{Pause: PauseBinaries{LinuxArm64: path}}
	bin, install, err := s.loadPauseBinary("linux", "arm64")
	if err != nil {
		t.Fatalf("loadPauseBinary: %v", err)
	}
	if string(bin) != "ARM" || install != "/.hpcc/pause" {
		t.Errorf("got (%q, %q), want (ARM, /.hpcc/pause)", bin, install)
	}
}

func TestLoadPauseBinary_windowsAmd64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pause.exe")
	if err := os.WriteFile(path, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Store{Pause: PauseBinaries{WindowsAmd64: path}}
	bin, install, err := s.loadPauseBinary("windows", "amd64")
	if err != nil {
		t.Fatalf("loadPauseBinary: %v", err)
	}
	if string(bin) != "MZ" {
		t.Errorf("bin = %q, want MZ", bin)
	}
	if install != `C:\.hpcc\pause.exe` {
		t.Errorf("install = %q, want C:\\.hpcc\\pause.exe", install)
	}
}

func TestLoadPauseBinary_unsupportedPlatform(t *testing.T) {
	s := &Store{}
	_, _, err := s.loadPauseBinary("plan9", "ppc64")
	if err == nil || !strings.Contains(err.Error(), "no pause binary configured") {
		t.Fatalf("expected 'no pause binary configured' error, got %v", err)
	}
}

func TestLoadPauseBinary_pathNotSet(t *testing.T) {
	s := &Store{}
	_, _, err := s.loadPauseBinary("linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("expected 'is not set' error, got %v", err)
	}
}

func TestLoadPauseBinary_fileMissing(t *testing.T) {
	s := &Store{Pause: PauseBinaries{LinuxAmd64: "/no/such/pause"}}
	_, _, err := s.loadPauseBinary("linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "read pause binary") {
		t.Fatalf("expected 'read pause binary' error, got %v", err)
	}
}

// --- helpers -------------------------------------------------------------

type tarEntry struct {
	hdr  *tar.Header
	body []byte
}

func readTar(t *testing.T, data []byte) map[string]tarEntry {
	t.Helper()
	out := map[string]tarEntry{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body for %q: %v", hdr.Name, err)
		}
		out[hdr.Name] = tarEntry{hdr: hdr, body: body}
	}
	return out
}

func contains(m map[string]tarEntry, name string) bool {
	_, ok := m[name]
	return ok
}

// dump map keys for diagnostics.
func sortedKeys(m map[string]tarEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var _ = sortedKeys // kept for diagnostic use during test failure investigation
