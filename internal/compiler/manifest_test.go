package compiler

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
)


// fakeCompiler is a minimal Compiler used by manifest / cache-key
// tests to drive BuildManifest without spawning a real toolchain.
// Only the methods CacheKey / BuildManifest touch are implemented;
// the rest panic so accidental use lights up loudly.
type fakeCompiler struct {
	identity []byte
	deps     []string
}

func (f *fakeCompiler) Name() string                                             { return "fake" }
func (f *fakeCompiler) Family() enum.Family                                      { return enum.GNUFamily }
func (f *fakeCompiler) Parse(args []string) (*Invocation, error)                 { panic("unused") }
func (f *fakeCompiler) Preprocess(inv *Invocation) (*PreprocessResult, error)    { panic("unused") }
func (f *fakeCompiler) Invoke(inv *Invocation) (*InvocationResult, error)        { panic("unused") }
func (f *fakeCompiler) FindDependencies(inv *Invocation) ([]string, error)       { return f.deps, nil }
func (f *fakeCompiler) Identity() ([]byte, error)                                { return f.identity, nil }
func (f *fakeCompiler) RewriteForPreprocessed(inv *Invocation, srcPath string) (*Invocation, error) {
	panic("unused")
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestBuildManifest_stableAcrossDepOrder(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "main.c", "int main(void){return 0;}\n")
	hdrA := writeFile(t, dir, "a.h", "#define A 1\n")
	hdrB := writeFile(t, dir, "b.h", "#define B 2\n")

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode}

	m1, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{deps: []string{hdrA, hdrB}}})
	if err != nil {
		t.Fatalf("BuildManifest #1: %v", err)
	}
	m2, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{deps: []string{hdrB, hdrA}}})
	if err != nil {
		t.Fatalf("BuildManifest #2: %v", err)
	}

	if m1.Digest != m2.Digest {
		t.Errorf("manifest digest should not depend on dep order; got %x vs %x", m1.Digest, m2.Digest)
	}
}

func TestBuildManifest_contentChangeChangesDigest(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "main.c", "int main(void){return 0;}\n")
	hdr := writeFile(t, dir, "h.h", "#define X 1\n")

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode}
	ctx := &Context{Compiler: &fakeCompiler{deps: []string{hdr}}}

	before, err := BuildManifest(inv, ctx)
	if err != nil {
		t.Fatalf("BuildManifest before: %v", err)
	}
	if err := os.WriteFile(hdr, []byte("#define X 2\n"), 0o644); err != nil {
		t.Fatalf("rewrite header: %v", err)
	}
	after, err := BuildManifest(inv, ctx)
	if err != nil {
		t.Fatalf("BuildManifest after: %v", err)
	}

	if before.Digest == after.Digest {
		t.Errorf("changing a header's contents must change the manifest digest")
	}
}

func TestBuildManifest_pathSwapChangesDigest(t *testing.T) {
	// Two files with swapped contents must produce a different digest
	// than the unswapped version. Today's PreprocessRemote path (raw
	// concatenated bytes) would NOT catch this; the manifest design
	// must, since path is mixed in alongside content.
	dir := t.TempDir()
	src := writeFile(t, dir, "main.c", "int main(void){return 0;}\n")
	a1 := writeFile(t, dir, "a.h", "AAA\n")
	b1 := writeFile(t, dir, "b.h", "BBB\n")

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode}
	ctx := &Context{Compiler: &fakeCompiler{deps: []string{a1, b1}}}
	orig, err := BuildManifest(inv, ctx)
	if err != nil {
		t.Fatalf("BuildManifest orig: %v", err)
	}

	if err := os.WriteFile(a1, []byte("BBB\n"), 0o644); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	if err := os.WriteFile(b1, []byte("AAA\n"), 0o644); err != nil {
		t.Fatalf("rewrite b: %v", err)
	}
	swapped, err := BuildManifest(inv, ctx)
	if err != nil {
		t.Fatalf("BuildManifest swapped: %v", err)
	}

	if orig.Digest == swapped.Digest {
		t.Errorf("swapping contents between two paths must change the manifest digest")
	}
}

func TestBuildManifest_blobsSortedByPath(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "z.c", "int main(void){return 0;}\n")
	hdr1 := writeFile(t, dir, "a.h", "A\n")
	hdr2 := writeFile(t, dir, "m.h", "M\n")

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode}
	// Feed deps in reverse path order to verify sort happens.
	m, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{deps: []string{hdr2, hdr1}}})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	for i := 1; i < len(m.Blobs); i++ {
		if m.Blobs[i-1].Path > m.Blobs[i].Path {
			t.Errorf("blobs not sorted by path: %q > %q", m.Blobs[i-1].Path, m.Blobs[i].Path)
		}
	}
}

func TestBuildManifest_dedupesInputAlsoInDeps(t *testing.T) {
	// GNU -M echoes the source itself in its dep list. Make sure we
	// don't double-hash it.
	dir := t.TempDir()
	src := writeFile(t, dir, "main.c", "int main(void){return 0;}\n")

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode}
	m, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{deps: []string{src}}})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Blobs) != 1 {
		t.Errorf("expected 1 blob after dedupe, got %d", len(m.Blobs))
	}
}

func TestCacheKey_manifestDigestShortCircuitMatchesPreprocessRemote(t *testing.T) {
	// The contract: a worker holding a ManifestDigest must produce
	// the same cache key as a client running PreprocessRemote against
	// the same files. This is what makes CAS-mode dispatch poison-
	// resistant — the key is reproducible from the manifest digest
	// alone, no source bytes required.
	dir := t.TempDir()
	src := writeFile(t, dir, "main.c", "int main(void){return 0;}\n")
	hdr := writeFile(t, dir, "h.h", "#define X 1\n")

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode}
	identity := []byte("fake-compiler-v1")

	// Client-side: PreprocessRemote runs BuildManifest under the hood.
	clientCtx := &Context{
		Compiler: &fakeCompiler{identity: identity, deps: []string{hdr}},
		Config:   &config.Config{PreprocessingMode: enum.PreprocessRemote},
	}
	clientKey, err := inv.CacheKey(clientCtx)
	if err != nil {
		t.Fatalf("client CacheKey: %v", err)
	}

	// Worker-side: re-verify the manifest, then short-circuit via
	// ManifestDigest. Same digest in must produce same key out.
	m, err := BuildManifest(inv, clientCtx)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	workerInv := *inv
	workerInv.ManifestDigest = &m.Digest
	workerCtx := &Context{
		Compiler: &fakeCompiler{identity: identity},
		Config:   &config.Config{PreprocessingMode: enum.PreprocessLocal},
	}
	workerKey, err := workerInv.CacheKey(workerCtx)
	if err != nil {
		t.Fatalf("worker CacheKey: %v", err)
	}

	if !bytes.Equal(clientKey, workerKey) {
		t.Errorf("client (PreprocessRemote) and worker (ManifestDigest short-circuit) must produce equal keys\n  client: %x\n  worker: %x", clientKey, workerKey)
	}
}

// A previous draft of this file pinned the (intentional) collision
// between a ManifestDigest-derived cache key and a PreprocessedDigest-
// derived one when both fields held the same 32 bytes, flagged as an
// open question in cas.md. Storage-layer namespacing (cache/compile/
// vs cache/source/ vs cache/manifest/) makes that collision moot at
// the layer that matters: even if the cache key bytes coincide, the
// entries can't land in the same store directory. Removed rather
// than left as a stale pin.

// --- path normalization / .hpcc discovery (Step 5a) ---

func TestFindProjectRoot_findsAtStartDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".hpcc"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	got := FindProjectRoot(dir)
	// Symlink resolution via filepath.EvalSymlinks normalizes paths
	// like macOS's /var → /private/var; compare resolved forms.
	wantResolved, _ := filepath.EvalSymlinks(dir)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("FindProjectRoot(%q) = %q, want %q", dir, got, dir)
	}
}

func TestFindProjectRoot_walksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hpcc"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := FindProjectRoot(deep)
	wantResolved, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("FindProjectRoot(deep) = %q, want %q", got, root)
	}
}

func TestFindProjectRoot_noMarkerReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := FindProjectRoot(dir); got != "" {
		t.Errorf("FindProjectRoot returned %q for tree without .hpcc; want empty", got)
	}
}

func TestNormalizeManifestPath_relativizesUnderRoot(t *testing.T) {
	root := "/home/alice/proj"
	got := normalizeManifestPath("/home/alice/proj/src/main.c", root)
	want := filepath.Join("src", "main.c")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeManifestPath_keepsSystemPathsAbsolute(t *testing.T) {
	root := "/home/alice/proj"
	got := normalizeManifestPath("/usr/include/stdio.h", root)
	if got != "/usr/include/stdio.h" {
		t.Errorf("system header path was rewritten to %q; should stay absolute", got)
	}
}

func TestNormalizeManifestPath_emptyRootIsNoOp(t *testing.T) {
	got := normalizeManifestPath("/home/alice/proj/src/main.c", "")
	if got != "/home/alice/proj/src/main.c" {
		t.Errorf("got %q, want input unchanged", got)
	}
}

func TestBuildManifest_normalizesPathsUnderHpccMarker(t *testing.T) {
	// Two parallel projects with the same logical layout but different
	// absolute prefixes. With a .hpcc marker at each project root and
	// identical file contents, the manifest digests must match —
	// that's what makes cross-developer cache hits land.
	make := func(t *testing.T) (root, srcPath string) {
		t.Helper()
		root = t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".hpcc"), nil, 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
			t.Fatalf("mkdir src: %v", err)
		}
		srcPath = filepath.Join(root, "src", "main.c")
		if err := os.WriteFile(srcPath, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
			t.Fatalf("write src: %v", err)
		}
		return
	}

	rootA, srcA := make(t)
	rootB, srcB := make(t)

	invA := &Invocation{Inputs: []string{srcA}, Mode: enum.CompileMode, Cwd: rootA}
	invB := &Invocation{Inputs: []string{srcB}, Mode: enum.CompileMode, Cwd: rootB}

	mA, err := BuildManifest(invA, &Context{Compiler: &fakeCompiler{}})
	if err != nil {
		t.Fatalf("BuildManifest A: %v", err)
	}
	mB, err := BuildManifest(invB, &Context{Compiler: &fakeCompiler{}})
	if err != nil {
		t.Fatalf("BuildManifest B: %v", err)
	}

	if mA.Digest != mB.Digest {
		t.Errorf("manifests should match across project roots; got %x vs %x", mA.Digest, mB.Digest)
	}
	// And the recorded path should be project-relative, not absolute.
	if len(mA.Blobs) != 1 {
		t.Fatalf("expected 1 blob, got %d", len(mA.Blobs))
	}
	wantPath := filepath.Join("src", "main.c")
	if mA.Blobs[0].Path != wantPath {
		t.Errorf("normalized path = %q, want %q", mA.Blobs[0].Path, wantPath)
	}
	_ = rootA
	_ = rootB
}

func TestBuildManifest_systemHeadersStayAbsolute(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hpcc"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := filepath.Join(root, "src", "main.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	// Simulate a system header by placing it outside the project root.
	sysRoot := t.TempDir()
	sysHdr := filepath.Join(sysRoot, "stdio.h")
	if err := os.WriteFile(sysHdr, []byte("/* stub */\n"), 0o644); err != nil {
		t.Fatalf("write sys: %v", err)
	}

	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode, Cwd: root}
	m, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{deps: []string{sysHdr}}})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	var sawProjectRelative, sawSystemAbsolute bool
	for _, b := range m.Blobs {
		if b.Path == filepath.Join("src", "main.c") {
			sawProjectRelative = true
		}
		if filepath.IsAbs(b.Path) && b.Path == sysHdr {
			sawSystemAbsolute = true
		}
	}
	if !sawProjectRelative {
		t.Errorf("project source not normalized to relative; blobs=%v", m.Blobs)
	}
	if !sawSystemAbsolute {
		t.Errorf("system header not kept absolute; blobs=%v", m.Blobs)
	}
}

// Regression: kernel-style builds invoke gcc with relative paths
// (e.g. `-include include/generated/autoconf.h`) and `gcc -M` echoes
// those paths back as-is. BuildManifest must open them relative to
// inv.Cwd (where `make` is running) rather than the daemon's process
// cwd — without that, every cacheable kernel TU silently falls back
// to local with "no such file or directory" on the dep file.
func TestBuildManifest_relativeDepResolvedAgainstInvCwd(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, ".hpcc"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "include", "generated"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := filepath.Join(projectDir, "main.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	hdr := filepath.Join(projectDir, "include", "generated", "autoconf.h")
	if err := os.WriteFile(hdr, []byte("#define FOO 1\n"), 0o644); err != nil {
		t.Fatalf("write hdr: %v", err)
	}

	// Switch process cwd to somewhere OTHER than projectDir so any
	// code path that uses process cwd for relative-path resolution
	// will fail to find the dep file. t.Chdir restores on test end
	// (Go 1.24+).
	other := t.TempDir()
	t.Chdir(other)

	inv := &Invocation{
		Inputs: []string{src}, // absolute; sidesteps the bug for the input itself
		Mode:   enum.CompileMode,
		Cwd:    projectDir,
	}
	// Dep returned by gcc -M as a RELATIVE path — the kernel build's
	// real shape. With the bug, hashFileBlob("include/generated/...")
	// would os.Open against process cwd (`other`) and ENOENT.
	deps := []string{filepath.Join("include", "generated", "autoconf.h")}

	m, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{deps: deps}})
	if err != nil {
		t.Fatalf("BuildManifest: %v (this is the kernel-bench regression)", err)
	}
	// Both inputs and the relative dep should be in the manifest.
	var sawHeader bool
	for _, b := range m.Blobs {
		if b.Path == filepath.Join("include", "generated", "autoconf.h") {
			sawHeader = true
		}
	}
	if !sawHeader {
		t.Errorf("manifest missing the relative dep; blobs=%v", m.Blobs)
	}
}

func TestBuildManifest_noMarkerKeepsAbsolutePaths(t *testing.T) {
	dir := t.TempDir() // no .hpcc marker
	src := filepath.Join(dir, "main.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	inv := &Invocation{Inputs: []string{src}, Mode: enum.CompileMode, Cwd: dir}

	m, err := BuildManifest(inv, &Context{Compiler: &fakeCompiler{}})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Blobs) != 1 {
		t.Fatalf("expected 1 blob, got %d", len(m.Blobs))
	}
	if !filepath.IsAbs(m.Blobs[0].Path) {
		t.Errorf("path %q should stay absolute when no .hpcc marker is present", m.Blobs[0].Path)
	}
}
