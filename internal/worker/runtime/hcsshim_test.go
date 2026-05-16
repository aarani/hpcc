package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These tests cover the parts of the hcsshim runtime that don't need a
// live containerd: option validation, the per-Exec staging dir layout,
// argv-translation rules, and the in-tree copyTree helper. The full
// container/task path is exercised by the hcsshim integration test
// behind the `integration` build tag.

func TestNewHcsshim_RequiresAddressAndRunDir(t *testing.T) {
	t.Run("address missing", func(t *testing.T) {
		_, err := NewHcsshim(HcsshimOptions{RunDir: "/tmp/x"})
		if err == nil {
			t.Fatal("expected error when address is empty")
		}
	})
	t.Run("run_dir missing", func(t *testing.T) {
		_, err := NewHcsshim(HcsshimOptions{Address: `\\.\pipe\x`})
		if err == nil {
			t.Fatal("expected error when run_dir is empty")
		}
	})
	t.Run("isolation typo rejected", func(t *testing.T) {
		_, err := NewHcsshim(HcsshimOptions{
			Address:   `\\.\pipe\x`,
			RunDir:    t.TempDir(),
			Isolation: "hyperV", // case-sensitive on purpose
		})
		if err == nil {
			t.Fatal("expected error for unknown isolation")
		}
		if !strings.Contains(err.Error(), "isolation") {
			t.Errorf("error %q should mention the bad knob", err)
		}
	})
}

func TestTranslateArgs_RewritesSrcAndOutRoots(t *testing.T) {
	argv := []string{
		"cl.exe",
		"/c",
		"/src/main.cpp",
		"/Fo/out/main.obj",
		"-DBUILD=/src-extra", // boundary-only: leftmost /src wins, /src-extra survives
	}
	srcRoot := `C:\src\exec123`
	outRoot := `C:\out\exec123`
	got := translateArgs(argv, srcRoot, outRoot)
	want := []string{
		"cl.exe",
		"/c",
		`C:\src\exec123/main.cpp`,
		"/Fo" + `C:\out\exec123/main.obj`,
		"-DBUILD=/src-extra",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("translateArgs mismatch\n got = %#v\nwant = %#v", got, want)
	}
}

func TestCopyTree_PreservesShapeAndContents(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	files := map[string]string{
		"a.txt":          "alpha",
		"sub/b.txt":      "bravo",
		"sub/nested/c.h": "charlie",
	}
	for rel, body := range files {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", full, err)
		}
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read %q: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%q body = %q, want %q", rel, got, want)
		}
	}
}

func TestCopyTree_SkipsSymlinksAndUnsupportedEntries(t *testing.T) {
	src := t.TempDir()
	target := filepath.Join(src, "real.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(src, "link")
	if err := os.Symlink(target, link); err != nil {
		// Some filesystems (Windows without dev mode) can't symlink — skip
		// rather than fail; the regular-file path is the contract this
		// test really cares about.
		t.Skipf("symlink unsupported: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "real.txt")); err != nil {
		t.Errorf("real.txt missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Errorf("link should have been skipped; lstat err = %v", err)
	}
}
