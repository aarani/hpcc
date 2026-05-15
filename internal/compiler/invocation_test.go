package compiler

import (
	"testing"

	"github.com/aarani/hpcc/internal/enum"
)

func TestReadsStdin(t *testing.T) {
	cases := []struct {
		name   string
		inputs []string
		want   bool
	}{
		{"bare dash", []string{"-"}, true},
		{"/dev/stdin", []string{"/dev/stdin"}, true},
		{"dash among many", []string{"foo.c", "-", "bar.c"}, true},
		{"plain file", []string{"foo.c"}, false},
		{"no inputs", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &Invocation{Inputs: tc.inputs}
			if got := inv.ReadsStdin(); got != tc.want {
				t.Errorf("ReadsStdin() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCacheable enumerates the rules from Invocation.Cacheable so a
// future change to the predicate has to touch this table — the
// runner and the daemon both rely on this being right.
func TestCacheable(t *testing.T) {
	cases := []struct {
		name string
		inv  *Invocation
		want bool
	}{
		{
			name: "single-input compile is cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}},
			want: true,
		},
		{
			name: "link mode is not cacheable",
			inv:  &Invocation{Mode: enum.LinkMode, Inputs: []string{"foo.o"}},
			want: false,
		},
		{
			name: "preprocess only is not cacheable",
			inv:  &Invocation{Mode: enum.PreprocessMode, Inputs: []string{"foo.c"}},
			want: false,
		},
		{
			name: "assemble only is not cacheable",
			inv:  &Invocation{Mode: enum.AssembleMode, Inputs: []string{"foo.c"}},
			want: false,
		},
		{
			name: "dep-only is not cacheable",
			inv:  &Invocation{Mode: enum.DepOnlyMode, Inputs: []string{"foo.c"}},
			want: false,
		},
		{
			name: "stdin source compile is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"-"}},
			want: false,
		},
		{
			name: "/dev/stdin source compile is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"/dev/stdin"}},
			want: false,
		},
		{
			name: "multi-input compile is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"a.c", "b.c"}},
			want: false,
		},
		{
			name: "zero-input compile is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: nil},
			want: false,
		},
		{
			name: "as-version.sh probe (/dev/null in+out) is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"/dev/null"}, Output: "/dev/null"},
			want: false,
		},
		{
			name: "cc-option probe (/dev/null in, real out) is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"/dev/null"}, Output: ".tmp/probe.o"},
			want: false,
		},
		{
			name: "compile discarding output is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}, Output: "/dev/null"},
			want: false,
		},
		{
			name: "uppercase .S assembly is not cacheable (may contain .incbin)",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.S"}, Output: "foo.o"},
			want: false,
		},
		{
			name: "lowercase .s assembly is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.s"}, Output: "foo.o"},
			want: false,
		},
		{
			name: "-x assembler input is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}, Output: "foo.o", Language: "assembler"},
			want: false,
		},
		{
			name: "-x assembler-with-cpp input is not cacheable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}, Output: "foo.o", Language: "assembler-with-cpp"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.Cacheable(); got != tc.want {
				t.Errorf("Cacheable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDispatchableUnderCAS pins the divergence from Cacheable: CAS
// dispatch can safely handle assembly inputs (the manifest captures
// the full closure including .incbin'd files), so .S/.s and -x
// assembler invocations are eligible. All other shape rules from
// Cacheable carry over unchanged.
func TestDispatchableUnderCAS(t *testing.T) {
	cases := []struct {
		name string
		inv  *Invocation
		want bool
	}{
		{
			name: "single-input compile is dispatchable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}},
			want: true,
		},
		{
			name: "uppercase .S assembly is dispatchable under CAS",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.S"}, Output: "foo.o"},
			want: true,
		},
		{
			name: "lowercase .s assembly is dispatchable under CAS",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.s"}, Output: "foo.o"},
			want: true,
		},
		{
			name: "-x assembler input is dispatchable under CAS",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}, Output: "foo.o", Language: "assembler"},
			want: true,
		},
		{
			name: "-x assembler-with-cpp input is dispatchable under CAS",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.c"}, Output: "foo.o", Language: "assembler-with-cpp"},
			want: true,
		},
		// Non-shape exclusions still apply: link/multi-input/stdin/probe.
		{
			name: "link mode is not dispatchable",
			inv:  &Invocation{Mode: enum.LinkMode, Inputs: []string{"foo.o"}},
			want: false,
		},
		{
			name: "stdin source is not dispatchable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"-"}},
			want: false,
		},
		{
			name: "multi-input is not dispatchable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"a.c", "b.c"}},
			want: false,
		},
		{
			name: "/dev/null probe is not dispatchable",
			inv:  &Invocation{Mode: enum.CompileMode, Inputs: []string{"/dev/null"}, Output: "tmp.o"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.DispatchableUnderCAS(); got != tc.want {
				t.Errorf("DispatchableUnderCAS() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCacheableAndCASDivergeOnAssembly pins the specific contract
// that powers Step 8: an assembly invocation isn't Cacheable (local
// cache key is unsound) but IS DispatchableUnderCAS (CAS handles it
// correctly). If both flip in the same direction, the carve-out
// behavior changes silently.
func TestCacheableAndCASDivergeOnAssembly(t *testing.T) {
	asm := &Invocation{Mode: enum.CompileMode, Inputs: []string{"foo.S"}, Output: "foo.o"}
	if asm.Cacheable() {
		t.Errorf("Cacheable() for .S should be false (cache key would be unsound)")
	}
	if !asm.DispatchableUnderCAS() {
		t.Errorf("DispatchableUnderCAS() for .S should be true (worker handles closure)")
	}
}
