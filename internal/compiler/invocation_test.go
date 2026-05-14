package compiler

import (
	"testing"

	"github.com/aarani/hpcc/internal/enum"
)

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.Cacheable(); got != tc.want {
				t.Errorf("Cacheable() = %v, want %v", got, tc.want)
			}
		})
	}
}
