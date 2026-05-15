package runner

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aarani/hpcc/internal/cache"
	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
)

func clangAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not found in PATH")
	}
}

func setupContext(t *testing.T) *compiler.Context {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	ds, err := store.NewDiskCacheStore(cacheDir, "100M")
	if err != nil {
		t.Fatal(err)
	}

	c, err := compiler.Detect("clang")
	if err != nil {
		t.Fatal(err)
	}

	ctx := &compiler.Context{
		Compiler: c,
		Config:   &config.Config{SourceMode: enum.SourceModeCAS},
	}
	ctx.Cache = cache.NewCompileCache(ctx, []store.Store{ds})
	return ctx
}

func writeSource(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCacheMissThenHit(t *testing.T) {
	clangAvailable(t)
	ctx := setupContext(t)

	dir := t.TempDir()
	src := writeSource(t, dir, "hello.c", "int main(void) { return 0; }\n")
	out := filepath.Join(dir, "hello.o")

	args := []string{"-c", src, "-o", out}

	inv, err := ctx.Compiler.Parse(args)
	if err != nil {
		t.Fatal(err)
	}

	// First lookup: cache miss.
	result, err := ctx.Cache.Lookup(inv)
	if err != nil {
		t.Fatalf("first Lookup error: %v", err)
	}
	if result != nil {
		t.Fatal("expected cache miss on first lookup")
	}

	// Invoke the compiler and store the result.
	result, err = ctx.Compiler.Invoke(inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("compile failed: exit %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	origOutput := make([]byte, len(result.Output))
	copy(origOutput, result.Output)

	if err := ctx.Cache.Store(inv, result); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Remove the object file so the hit must come from cache.
	os.Remove(out)

	// Second lookup: cache hit.
	cached, err := ctx.Cache.Lookup(inv)
	if err != nil {
		t.Fatalf("second Lookup error: %v", err)
	}
	if cached == nil {
		t.Fatal("expected cache hit on second lookup")
	}
	if cached.ExitCode != 0 {
		t.Errorf("cached ExitCode = %d, want 0", cached.ExitCode)
	}

	// The cache hit should have returned the output bytes in
	// cached.Output. The caller (runner.Run / daemon.handleRequest /
	// worker.respond) is responsible for materializing them to
	// disk; loadEntry deliberately doesn't write to disk itself
	// because outputPath on the worker side is an in-VM staging
	// path that doesn't exist on the host.
	if !bytes.Equal(cached.Output, origOutput) {
		t.Errorf("cached.Output (%d bytes) differs from original (%d bytes)", len(cached.Output), len(origOutput))
	}
}

func TestCacheInvalidatedBySourceChange(t *testing.T) {
	clangAvailable(t)
	ctx := setupContext(t)

	dir := t.TempDir()
	src := writeSource(t, dir, "value.c", "int value(void) { return 1; }\n")
	out := filepath.Join(dir, "value.o")
	args := []string{"-c", src, "-o", out}

	// Compile and cache version 1.
	inv1, err := ctx.Compiler.Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	res1, err := ctx.Compiler.Invoke(inv1)
	if err != nil {
		t.Fatal(err)
	}
	if res1.ExitCode != 0 {
		t.Fatalf("compile v1 failed: %s", res1.Stderr)
	}
	if err := ctx.Cache.Store(inv1, res1); err != nil {
		t.Fatal(err)
	}

	// Change the source file.
	writeSource(t, dir, "value.c", "int value(void) { return 2; }\n")

	// Re-parse (same args, but source content changed).
	inv2, err := ctx.Compiler.Parse(args)
	if err != nil {
		t.Fatal(err)
	}

	// Lookup should miss because the cache key includes preprocessed source.
	cached, err := ctx.Cache.Lookup(inv2)
	if err != nil {
		t.Fatalf("Lookup after source change: %v", err)
	}
	if cached != nil {
		t.Fatal("expected cache miss after source change, got hit")
	}
}

func TestCacheHitReplayStderr(t *testing.T) {
	clangAvailable(t)
	ctx := setupContext(t)

	dir := t.TempDir()
	src := writeSource(t, dir, "warn.c",
		"#include <stdio.h>\nint main(void) { int x; return x; }\n")
	out := filepath.Join(dir, "warn.o")
	args := []string{"-c", "-Wall", src, "-o", out}

	inv, err := ctx.Compiler.Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ctx.Compiler.Invoke(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Cache.Store(inv, result); err != nil {
		t.Fatal(err)
	}

	os.Remove(out)

	cached, err := ctx.Cache.Lookup(inv)
	if err != nil {
		t.Fatal(err)
	}
	if cached == nil {
		t.Fatal("expected cache hit")
	}
	if !bytes.Equal(cached.Stderr, result.Stderr) {
		t.Errorf("cached stderr differs:\n  got:  %q\n  want: %q", cached.Stderr, result.Stderr)
	}
	if cached.ExitCode != result.ExitCode {
		t.Errorf("cached exit code = %d, want %d", cached.ExitCode, result.ExitCode)
	}
}

func TestCacheMissWithDifferentFlags(t *testing.T) {
	clangAvailable(t)
	ctx := setupContext(t)

	dir := t.TempDir()
	src := writeSource(t, dir, "flags.c", "int f(void) { return 0; }\n")
	outO0 := filepath.Join(dir, "flags-O0.o")
	outO2 := filepath.Join(dir, "flags-O2.o")

	// Compile with -O0.
	argsO0 := []string{"-c", "-O0", src, "-o", outO0}
	inv0, err := ctx.Compiler.Parse(argsO0)
	if err != nil {
		t.Fatal(err)
	}
	res0, err := ctx.Compiler.Invoke(inv0)
	if err != nil {
		t.Fatal(err)
	}
	if res0.ExitCode != 0 {
		t.Fatalf("compile -O0 failed: %s", res0.Stderr)
	}
	if err := ctx.Cache.Store(inv0, res0); err != nil {
		t.Fatal(err)
	}

	// Compile with -O2 — different flags should miss.
	argsO2 := []string{"-c", "-O2", src, "-o", outO2}
	inv2, err := ctx.Compiler.Parse(argsO2)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := ctx.Cache.Lookup(inv2)
	if err != nil {
		t.Fatalf("Lookup -O2: %v", err)
	}
	if cached != nil {
		t.Fatal("expected cache miss for different optimization level, got hit")
	}
}
