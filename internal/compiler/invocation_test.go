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

// TestHasUncapturedSideEffectFlag pins the curated list of gcc flags
// that bypass dispatch+cache so users keep the side-effect files they
// asked for (.dwo, .ci, .gcno, .ii dumps, …). Adding a flag to the
// helper should also add a row here so the predicate's surface area
// is reviewable.
func TestHasUncapturedSideEffectFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty argv", nil, false},
		{"normal compile", []string{"-c", "-O2", "foo.c", "-o", "foo.o"}, false},
		{"plain -save-temps", []string{"-save-temps", "-c", "foo.c"}, true},
		{"-save-temps=cwd", []string{"-save-temps=cwd", "-c", "foo.c"}, true},
		{"-save-temps=obj", []string{"-save-temps=obj", "-c", "foo.c"}, true},
		{"-fdump-tree-all", []string{"-fdump-tree-all", "-c", "foo.c"}, true},
		{"-fdump-rtl-all", []string{"-fdump-rtl-all", "-c", "foo.c"}, true},
		{"-fdump-ipa-all", []string{"-fdump-ipa-all", "-c", "foo.c"}, true},
		{"-fdump-passes", []string{"-fdump-passes", "-c", "foo.c"}, true},
		{"-fdump-go-spec=file", []string{"-fdump-go-spec=foo.spec", "-c", "foo.c"}, true},
		{"-fcallgraph-info", []string{"-fcallgraph-info", "-c", "foo.c"}, true},
		{"-fcallgraph-info=su,da", []string{"-fcallgraph-info=su,da", "-c", "foo.c"}, true},
		{"-fprofile-generate", []string{"-fprofile-generate", "-c", "foo.c"}, true},
		{"-fprofile-generate=path", []string{"-fprofile-generate=/tmp/p", "-c", "foo.c"}, true},
		{"-fprofile-arcs", []string{"-fprofile-arcs", "-c", "foo.c"}, true},
		{"-ftest-coverage", []string{"-ftest-coverage", "-c", "foo.c"}, true},
		{"--coverage", []string{"--coverage", "-c", "foo.c"}, true},
		{"-gsplit-dwarf", []string{"-c", "-gsplit-dwarf", "foo.c"}, true},
		{"-fdiagnostics-format=sarif-file", []string{"-fdiagnostics-format=sarif-file", "-c", "foo.c"}, true},
		{"-fdiagnostics-format=json-file", []string{"-fdiagnostics-format=json-file", "-c", "foo.c"}, true},
		{"-fdiagnostics-format=text (default)", []string{"-fdiagnostics-format=text", "-c", "foo.c"}, false},

		// Common kernel flags should NOT trigger — pinning the false
		// negatives matters more than the positives here.
		{"kernel cflags don't trigger", []string{
			"-c", "-Wp,-MMD,foo.d", "-nostdinc", "-isystem", "/x",
			"-O2", "-Wall", "-Werror", "-fno-strict-aliasing", "foo.c",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasUncapturedSideEffectFlag(tc.args); got != tc.want {
				t.Errorf("HasUncapturedSideEffectFlag(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestCacheable_rejectsUncapturedSideEffectFlags confirms the gate
// flows through Cacheable (and therefore DispatchableUnderCAS, via
// their shared cacheableShape predicate).
func TestCacheable_rejectsUncapturedSideEffectFlags(t *testing.T) {
	inv := &Invocation{
		Mode:    enum.CompileMode,
		Inputs:  []string{"foo.c"},
		RawArgs: []string{"-c", "-save-temps", "foo.c", "-o", "foo.o"},
	}
	if inv.Cacheable() {
		t.Errorf("Cacheable() should be false for -save-temps invocation")
	}
	if inv.DispatchableUnderCAS() {
		t.Errorf("DispatchableUnderCAS() should be false for -save-temps invocation")
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
