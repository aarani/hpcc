package compiler

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRewritePathPrefix_JoinedAndSeparate(t *testing.T) {
	inv, err := ParseGNU([]string{
		"-c",
		"-I/home/alice/proj/include",
		"-isystem", "/home/alice/proj/third_party/include",
		"-DBAR=1",
		"-o", "/home/alice/proj/build/foo.o",
		"/home/alice/proj/src/foo.cpp",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := RewritePathPrefix(inv, "/home/alice/proj", "/src")
	want := []string{
		"-c",
		"-I/src/include",
		"-isystem", "/src/third_party/include",
		"-DBAR=1",
		"-o", "/src/build/foo.o",
		"/src/src/foo.cpp",
	}
	if !reflect.DeepEqual(got.RawArgs, want) {
		t.Fatalf("RawArgs:\n got:  %q\n want: %q", got.RawArgs, want)
	}
	if got.Output != "/src/build/foo.o" {
		t.Errorf("Output = %q, want /src/build/foo.o", got.Output)
	}
	if !slices.Equal(got.Inputs, []string{"/src/src/foo.cpp"}) {
		t.Errorf("Inputs = %v, want [/src/src/foo.cpp]", got.Inputs)
	}
	if !slices.Equal(got.Includes, []string{"/src/include"}) {
		t.Errorf("Includes = %v", got.Includes)
	}
}

func TestRewritePathPrefix_SiblingDirectoryNotMatched(t *testing.T) {
	// /home/alice/proj-other should NOT be rewritten when host is /home/alice/proj.
	inv, err := ParseGNU([]string{"-I/home/alice/proj-other/include", "/home/alice/proj/main.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := RewritePathPrefix(inv, "/home/alice/proj", "/src")
	want := []string{"-I/home/alice/proj-other/include", "/src/main.cpp"}
	if !reflect.DeepEqual(got.RawArgs, want) {
		t.Fatalf("RawArgs:\n got:  %q\n want: %q", got.RawArgs, want)
	}
}

func TestRewritePathPrefix_ExactRootMatch(t *testing.T) {
	// A bare reference to the project root itself should rewrite.
	inv := &Invocation{RawArgs: []string{"-I/home/alice/proj", "-c", "/home/alice/proj/main.cpp"}}
	got := RewritePathPrefix(inv, "/home/alice/proj", "/src")
	want := []string{"-I/src", "-c", "/src/main.cpp"}
	if !reflect.DeepEqual(got.RawArgs, want) {
		t.Fatalf("RawArgs:\n got:  %q\n want: %q", got.RawArgs, want)
	}
}

func TestRewritePathPrefix_DoesNotMutateInput(t *testing.T) {
	original := []string{"-I/host/x", "/host/main.cpp"}
	inv := &Invocation{RawArgs: slices.Clone(original)}
	_ = RewritePathPrefix(inv, "/host", "/vm")
	if !slices.Equal(inv.RawArgs, original) {
		t.Errorf("input mutated: %v", inv.RawArgs)
	}
}

func TestRewriteForPreprocessed_DropsIncludesDefinesPassthrough(t *testing.T) {
	c := &clangCompiler{name: "clang"}
	inv, err := ParseGNU([]string{
		"-c",
		"-I/src/include",
		"-isystem", "/src/sys",
		"-DFOO=1",
		"-include", "prefix.h",
		"-MD", "-MF", "deps.d",
		"-Wl,--as-needed",
		"-O2", "-g", "-Wall", "-fPIC", "-march=native",
		"-std=c++17",
		"-o", "/out/foo.o",
		"/src/foo.cpp",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	want := []string{
		"-x", "c++-cpp-output",
		"-c", "/staged/foo.i",
		"-o", "/out/foo.o",
		"-O2", "-g", "-Wall", "-fPIC", "-march=native",
		"-std=c++17",
	}
	if !reflect.DeepEqual(got.RawArgs, want) {
		t.Fatalf("RawArgs:\n got:  %q\n want: %q", got.RawArgs, want)
	}

	// Sanity-check that the dropped flags really aren't present.
	for _, drop := range []string{"-I/src/include", "-isystem", "-DFOO=1", "-include", "-MD", "-MF", "-Wl,--as-needed"} {
		for _, a := range got.RawArgs {
			if a == drop {
				t.Errorf("expected %q to be dropped, still present", drop)
			}
		}
	}
}

func TestRewriteForPreprocessed_DefaultsToCForClang(t *testing.T) {
	c := &clangCompiler{name: "clang"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[1] != "cpp-output" {
		t.Errorf("expected -x cpp-output for clang+.c, got -x %q (full: %v)", got.RawArgs[1], got.RawArgs)
	}
}

func TestRewriteForPreprocessed_PicksCxxFromExtension(t *testing.T) {
	c := &clangCompiler{name: "clang"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.cc"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[1] != "c++-cpp-output" {
		t.Errorf("expected -x c++-cpp-output for .cc input, got -x %q", got.RawArgs[1])
	}
}

// TestRewriteForPreprocessed_PicksAssemblerFromExtension pins the
// fix for the .S regression: before this, the rewriter labeled
// preprocessed assembly as "cpp-output" (C source), and the
// downstream worker compile cascaded with "stray '@' in program" /
// "invalid suffix 'b' on integer constant" / "expected identifier
// or '(' before '.' token". The Linux kernel build has dozens of
// .S files (efi-mixed.S, initramfs_data.S, asm-offsets, etc.) so
// the bug killed every FC-mode kernel build the moment one was
// dispatched.
func TestRewriteForPreprocessed_PicksAssemblerFromExtension(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"uppercase .S", "/src/efi-mixed.S"},
		{"lowercase .s (already preprocessed)", "/src/foo.s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clangCompiler{name: "gcc"}
			inv, err := ParseGNU([]string{"-c", tc.input})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := c.RewriteForPreprocessed(inv, "/staged/foo.s")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if got.RawArgs[1] != "assembler" {
				t.Errorf("expected -x assembler for %s, got -x %q (full: %v)",
					tc.input, got.RawArgs[1], got.RawArgs)
			}
		})
	}
}

// TestRewriteForPreprocessed_PicksAssemblerFromXFlag covers the
// explicit `-x assembler-with-cpp` path the kernel passes for .S
// files where the make rule wants to force the language regardless
// of extension.
func TestRewriteForPreprocessed_PicksAssemblerFromXFlag(t *testing.T) {
	c := &clangCompiler{name: "gcc"}
	inv, err := ParseGNU([]string{"-c", "-x", "assembler-with-cpp", "/src/foo.S"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.s")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[1] != "assembler" {
		t.Errorf("expected -x assembler for -x assembler-with-cpp input, got -x %q",
			got.RawArgs[1])
	}
}

func TestRewriteForPreprocessed_PicksCxxFromClangPlusPlus(t *testing.T) {
	c := &clangCompiler{name: "clang++"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[1] != "c++-cpp-output" {
		t.Errorf("expected -x c++-cpp-output for clang++, got -x %q", got.RawArgs[1])
	}
}

func TestRewriteForPreprocessed_PicksCxxFromCxxDriver(t *testing.T) {
	c := &clangCompiler{name: "c++"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[1] != "c++-cpp-output" {
		t.Errorf("expected -x c++-cpp-output for c++ driver, got -x %q", got.RawArgs[1])
	}
}

func TestRewriteForPreprocessed_DefaultsToCForCcDriver(t *testing.T) {
	c := &clangCompiler{name: "cc"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[1] != "cpp-output" {
		t.Errorf("expected -x cpp-output for cc+.c, got -x %q", got.RawArgs[1])
	}
}

func TestValidateNoHostPaths_AcceptsRewrittenArgv(t *testing.T) {
	clean := []string{
		"clang",
		"-c", "/src/main.cpp",
		"-I/src/include",
		"-isystem", "/usr/include",
		"-o", "/out/main.o",
		"-O2", "-Wall",
	}
	if err := ValidateNoHostPaths(clean); err != nil {
		t.Errorf("clean argv rejected: %v", err)
	}
}

func TestValidateNoHostPaths_FlagsLinuxHome(t *testing.T) {
	if err := ValidateNoHostPaths([]string{"clang", "-I/home/alice/proj/include", "/src/main.cpp"}); err == nil {
		t.Error("expected error for /home/ path, got nil")
	}
}

func TestValidateNoHostPaths_FlagsMacUsers(t *testing.T) {
	if err := ValidateNoHostPaths([]string{"clang", "/Users/alice/proj/main.cpp"}); err == nil {
		t.Error("expected error for /Users/ path, got nil")
	}
}

func TestValidateNoHostPaths_FlagsWindowsUsersAnyCase(t *testing.T) {
	cases := [][]string{
		{"cl.exe", `/IC:\Users\alice\proj\include`},
		{"cl.exe", `D:\users\bob\proj\main.cpp`},
	}
	for _, args := range cases {
		if err := ValidateNoHostPaths(args); err == nil {
			t.Errorf("expected error for Windows user home in %v, got nil", args)
		}
	}
}

func TestValidateNoHostPaths_DefineWithEmbeddedPath(t *testing.T) {
	// -DFOO=/home/alice/x is also a leak, even though it's "just" a value.
	if err := ValidateNoHostPaths([]string{"clang", "-DFOO=/home/alice/x", "/src/main.cpp"}); err == nil {
		t.Error("expected error for embedded host path in -D value, got nil")
	}
}

func TestValidateNoHostPaths_AcceptsSystemDirs(t *testing.T) {
	args := []string{
		"clang", "-c",
		"-I/usr/include",
		"-I/opt/toolchain/include",
		"-L/lib", "-L/lib64",
		"/src/main.cpp",
	}
	if err := ValidateNoHostPaths(args); err != nil {
		t.Errorf("system-dir paths rejected: %v", err)
	}
}

// TestRewriteForPreprocessed_InfersDefaultOutput pins that when the
// user omits -o on a compile, the parser infers gcc's default
// (basename + ".o") and the rewriter emits it explicitly. Without
// this, a preprocessed-compile dispatched to a remote worker would
// have nowhere to write its object file and the cache layer would
// have no path to read back.
func TestRewriteForPreprocessed_InfersDefaultOutput(t *testing.T) {
	c := &clangCompiler{name: "clang"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.Output != "foo.o" {
		t.Errorf("ParseGNU did not infer default output; got %q want %q", inv.Output, "foo.o")
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	found := false
	for i, a := range got.RawArgs {
		if a == "-o" && i+1 < len(got.RawArgs) && got.RawArgs[i+1] == "foo.o" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -o foo.o in rewritten argv, got %v", got.RawArgs)
	}
}

// TestRewriteForPreprocessed_StripsWerror pins the policy that
// `-Werror` and `-Werror=<class>` are dropped at the rewrite seam.
// hpcc's preprocessed-mode dispatch is a two-step compile by
// construction; gcc's "suppress inside macro expansion" heuristic
// for many warning classes depends on the preprocessor's in-memory
// macro table, which is lost across that seam. Honoring -Werror
// across the seam would silently fail builds on macro-heavy code
// (the Linux kernel being the obvious example) where local-mode
// gcc one-step would have produced clean objects from the same
// source. The warnings still emit — visible in build logs — they
// just don't fail the build. See gnuKeepForPreprocessed doc.
func TestRewriteForPreprocessed_StripsWerror(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"bare -Werror", []string{"-c", "-Werror", "/src/foo.c"}},
		{"-Werror=tautological-compare", []string{"-c", "-Werror=tautological-compare", "/src/foo.c"}},
		{"-Werror=address", []string{"-c", "-Werror=address", "/src/foo.c"}},
		{"-Werror=date-time (kernel reproducibility flag)", []string{"-c", "-Werror=date-time", "/src/foo.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clangCompiler{name: "gcc"}
			inv, err := ParseGNU(tc.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			for _, a := range got.RawArgs {
				if a == "-Werror" || strings.HasPrefix(a, "-Werror=") {
					t.Errorf("expected %q to be stripped, still present in %v", a, got.RawArgs)
				}
			}
		})
	}
}

// TestRewriteForPreprocessed_KeepsNonWerrorWarnings pins that the
// Werror strip doesn't accidentally eat regular -W flags.
func TestRewriteForPreprocessed_KeepsNonWerrorWarnings(t *testing.T) {
	c := &clangCompiler{name: "gcc"}
	inv, err := ParseGNU([]string{
		"-c", "-Wall", "-Wextra", "-Wno-unused", "-Werror", "/src/foo.c",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, want := range []string{"-Wall", "-Wextra", "-Wno-unused"} {
		found := false
		for _, a := range got.RawArgs {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q kept in rewritten argv: %v", want, got.RawArgs)
		}
	}
}

func TestRewriteForPreprocessed_MSVC_DropsIncludesDefinesPassthrough(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{
		"/c",
		"/Iinclude",
		"/external:I", "thirdparty",
		"/DFOO=1",
		"/U", "BAR",
		"/showIncludes",
		"/Fd:foo.pdb",
		"/Fa:foo.asm",
		"/O2", "/Zi", "/W4", "/std:c++20",
		"/MD", "/EHsc",
		"/Fo:out.obj",
		"foo.cpp",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	want := []string{
		"/c", "/nologo", `/TpC:\staged\foo.i`,
		"/Fo:out.obj",
		"/O2", "/Zi", "/W4", "/std:c++20",
		"/MD", "/EHsc",
	}
	if !reflect.DeepEqual(got.RawArgs, want) {
		t.Fatalf("RawArgs:\n got:  %q\n want: %q", got.RawArgs, want)
	}

	for _, drop := range []string{"/Iinclude", "/external:I", "/DFOO=1", "/showIncludes", "/Fd:foo.pdb", "/Fa:foo.asm"} {
		for _, a := range got.RawArgs {
			if a == drop {
				t.Errorf("expected %q to be dropped, still present", drop)
			}
		}
	}
}

func TestRewriteForPreprocessed_MSVC_DefaultsToCForCFile(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{"/c", "foo.c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[2] != `/TcC:\staged\foo.i` {
		t.Errorf("expected /Tc<path> for .c input, got %q (full: %v)", got.RawArgs[2], got.RawArgs)
	}
	if got.Language != "c" {
		t.Errorf("Language = %q, want c", got.Language)
	}
}

func TestRewriteForPreprocessed_MSVC_PicksCxxFromExtension(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{"/c", "foo.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[2] != `/TpC:\staged\foo.i` {
		t.Errorf("expected /Tp<path> for .cpp input, got %q (full: %v)", got.RawArgs[2], got.RawArgs)
	}
	if got.Language != "c++" {
		t.Errorf("Language = %q, want c++", got.Language)
	}
}

func TestRewriteForPreprocessed_MSVC_ExplicitTpForcesCxx(t *testing.T) {
	c := &clCompiler{name: "cl"}
	// /Tp foo.c forces foo.c to be C++; the rewriter should honor that.
	inv, err := ParseMSVC([]string{"/c", "/Tp", "foo.c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got.RawArgs[2] != `/TpC:\staged\foo.i` {
		t.Errorf("expected /Tp<path> when invocation forced /Tp, got %q", got.RawArgs[2])
	}
}

func TestRewriteForPreprocessed_MSVC_InfersDefaultOutput(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{"/c", "foo.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inv.Output != "foo.obj" {
		t.Errorf("ParseMSVC did not infer default output; got %q want %q", inv.Output, "foo.obj")
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	found := false
	for _, a := range got.RawArgs {
		if a == "/Fo:foo.obj" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected /Fo:foo.obj in rewritten argv, got %v", got.RawArgs)
	}
}

func TestRewriteForPreprocessed_MSVC_UnknownFlagKept(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{"/c", "/some-unknown-flag", "foo.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	found := false
	for _, a := range got.RawArgs {
		if a == "/some-unknown-flag" {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown flag was dropped, got %v", got.RawArgs)
	}
}

func TestRewriteForPreprocessed_MSVC_LinkTailDropped(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{"/c", "/O2", "foo.cpp", "/link", "/DEBUG", "lib.lib"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, a := range got.RawArgs {
		if a == "/link" || a == "/DEBUG" || a == "lib.lib" {
			t.Errorf("expected /link tail to be dropped, found %q in %v", a, got.RawArgs)
		}
	}
}
