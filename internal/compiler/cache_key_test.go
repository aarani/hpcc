package compiler

import (
	"bytes"
	"testing"

	"github.com/aarani/hpcc/internal/enum"
)

func TestCacheKeyFlags_definesOrderInvariant(t *testing.T) {
	a := &Invocation{Defines: map[string]string{"FOO": "1", "BAR": "2"}, Mode: enum.CompileMode}
	b := &Invocation{Defines: map[string]string{"BAR": "2", "FOO": "1"}, Mode: enum.CompileMode}
	if !bytes.Equal(cacheKeyFlags(a), cacheKeyFlags(b)) {
		t.Errorf("define order should not affect cache key")
	}
}

func TestCacheKeyFlags_includeOrderMatters(t *testing.T) {
	a := &Invocation{Includes: []string{"a", "b"}, Mode: enum.CompileMode}
	b := &Invocation{Includes: []string{"b", "a"}, Mode: enum.CompileMode}
	if bytes.Equal(cacheKeyFlags(a), cacheKeyFlags(b)) {
		t.Errorf("include search order should affect cache key")
	}
}

func TestCacheKeyFlags_optimChanges(t *testing.T) {
	a := &Invocation{Optim: "2", Mode: enum.CompileMode}
	b := &Invocation{Optim: "0", Mode: enum.CompileMode}
	if bytes.Equal(cacheKeyFlags(a), cacheKeyFlags(b)) {
		t.Errorf("-O0 and -O2 should produce different cache keys")
	}
}

func TestCacheKeyFlags_modeChanges(t *testing.T) {
	a := &Invocation{Mode: enum.CompileMode}
	b := &Invocation{Mode: enum.AssembleMode}
	if bytes.Equal(cacheKeyFlags(a), cacheKeyFlags(b)) {
		t.Errorf("compile vs assemble should produce different cache keys")
	}
}

func TestCacheKeyFlags_outputIgnored(t *testing.T) {
	a := &Invocation{Output: "foo.o", Mode: enum.CompileMode}
	b := &Invocation{Output: "bar.o", Mode: enum.CompileMode}
	if !bytes.Equal(cacheKeyFlags(a), cacheKeyFlags(b)) {
		t.Errorf("output path should not affect cache key")
	}
}

func TestFilterIrrelevantPassthrough(t *testing.T) {
	in := []string{
		"-MD", "-MMD",
		"-MF", "deps.d",
		"-MT", "target.o",
		"-Wl,--as-needed",
		"-include", "prefix.h",
		"/showIncludes",
	}
	got := filterIrrelevantPassthrough(in)
	want := []string{"-Wl,--as-needed", "-include", "prefix.h"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestCacheKeyFlags_depInfoIgnored(t *testing.T) {
	a := &Invocation{
		Mode:        enum.CompileMode,
		Passthrough: []string{"-Wl,--as-needed"},
	}
	b := &Invocation{
		Mode:        enum.CompileMode,
		Passthrough: []string{"-MD", "-MF", "deps.d", "-Wl,--as-needed"},
	}
	if !bytes.Equal(cacheKeyFlags(a), cacheKeyFlags(b)) {
		t.Errorf("dep-info passthrough should not affect cache key")
	}
}
