package compiler

import (
	"reflect"
	"testing"

	"github.com/aarani/hpcc/internal/enum"
)

func TestParseGNU_basicCompile(t *testing.T) {
	inv, err := ParseGNU([]string{"-c", "foo.c", "-o", "foo.o"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Mode != enum.CompileMode {
		t.Errorf("Mode = %v, want CompileMode", inv.Mode)
	}
	if !reflect.DeepEqual(inv.Inputs, []string{"foo.c"}) {
		t.Errorf("Inputs = %v, want [foo.c]", inv.Inputs)
	}
	if inv.Output != "foo.o" {
		t.Errorf("Output = %q, want foo.o", inv.Output)
	}
}

func TestParseGNU_includesBothForms(t *testing.T) {
	inv, err := ParseGNU([]string{
		"-Iinclude",
		"-I", "/usr/local/include",
		"-isystem", "/opt/sysroot/include",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"include", "/usr/local/include"}
	if !reflect.DeepEqual(inv.Includes, want) {
		t.Errorf("Includes = %v, want %v", inv.Includes, want)
	}
	if !reflect.DeepEqual(inv.SystemIncludes, []string{"/opt/sysroot/include"}) {
		t.Errorf("SystemIncludes = %v", inv.SystemIncludes)
	}
}

func TestParseGNU_definesAndStd(t *testing.T) {
	inv, err := ParseGNU([]string{"-DFOO", "-DBAR=42", "-std=c++20"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Defines["FOO"] != "" || inv.Defines["BAR"] != "42" {
		t.Errorf("Defines = %v", inv.Defines)
	}
	if inv.Std != "c++20" {
		t.Errorf("Std = %q", inv.Std)
	}
}

func TestParseGNU_longestPrefixWins(t *testing.T) {
	// "-Wl,--as-needed" must match "-Wl," (passthrough), not "-W" (warning).
	inv, err := ParseGNU([]string{"-Wall", "-Wl,--as-needed"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Warnings, []string{"all"}) {
		t.Errorf("Warnings = %v, want [all]", inv.Warnings)
	}
	if !reflect.DeepEqual(inv.Passthrough, []string{"-Wl,--as-needed"}) {
		t.Errorf("Passthrough = %v", inv.Passthrough)
	}
}

func TestParseGNU_unknownPassesThrough(t *testing.T) {
	inv, err := ParseGNU([]string{"-c", "-fnonsense-from-the-future", "x.c"})
	if err != nil {
		t.Fatal(err)
	}
	// "-f" is JoinedValue, so this lands in Features, not Unknown.
	if !reflect.DeepEqual(inv.Features, []string{"nonsense-from-the-future"}) {
		t.Errorf("Features = %v", inv.Features)
	}
	// A truly unrecognized prefix:
	inv, err = ParseGNU([]string{"--unknown-flag", "x.c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Unknown, []string{"--unknown-flag"}) {
		t.Errorf("Unknown = %v", inv.Unknown)
	}
}

func TestParseGNU_doubleDashStopsParsing(t *testing.T) {
	inv, err := ParseGNU([]string{"-c", "--", "-weird-filename.c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Inputs, []string{"-weird-filename.c"}) {
		t.Errorf("Inputs = %v", inv.Inputs)
	}
	if len(inv.Unknown) != 0 {
		t.Errorf("Unknown should be empty, got %v", inv.Unknown)
	}
}

func TestParseGNU_defaultsToLink(t *testing.T) {
	inv, err := ParseGNU([]string{"foo.o", "bar.o", "-o", "prog"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Mode != enum.LinkMode {
		t.Errorf("Mode = %v, want LinkMode", inv.Mode)
	}
}

// TestParseGNU_inferDefaultOutput pins the gcc convention that
// `gcc -c path/to/foo.c` (no -o) writes `foo.o` in cwd. Hpcc has to
// know this so the cache can read the file back on miss and write
// it out on hit; previously inv.Output stayed empty and a cache hit
// silently failed to materialize the object file after a clean.
func TestParseGNU_inferDefaultOutput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bare basename", []string{"-c", "foo.c"}, "foo.o"},
		{"relative dir is stripped", []string{"-c", "sub/foo.c"}, "foo.o"},
		{"absolute dir is stripped", []string{"-c", "/abs/path/foo.c"}, "foo.o"},
		{"c++ extension", []string{"-c", "src/foo.cpp"}, "foo.o"},
		{"unknown extension still trims last dot", []string{"-c", "foo.weird"}, "foo.o"},
		{"explicit -o not overridden", []string{"-c", "foo.c", "-o", "build/x.o"}, "build/x.o"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := ParseGNU(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if inv.Output != tc.want {
				t.Errorf("Output = %q, want %q", inv.Output, tc.want)
			}
		})
	}
}

// TestParseGNU_inferOutputOnlyForSingleInputCompile guards the modes
// and shapes where inference must NOT fire — link mode (a.out is
// gcc's job, not the cache's), multi-input compiles (one .o per
// input doesn't fit CompileCache's single-output shape), preprocess and
// assemble modes (uncached, compiler handles its own defaults), and
// stdin (no input name to base a default on).
func TestParseGNU_inferOutputOnlyForSingleInputCompile(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"link mode (no -c)", []string{"foo.c"}},
		{"multi-input compile", []string{"-c", "foo.c", "bar.c"}},
		{"preprocess only", []string{"-E", "foo.c"}},
		{"assemble only", []string{"-S", "foo.c"}},
		{"stdin source", []string{"-c", "-x", "c", "-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := ParseGNU(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if inv.Output != "" {
				t.Errorf("Output = %q, want empty", inv.Output)
			}
		})
	}
}
