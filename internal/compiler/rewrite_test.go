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

func TestRewriteForPreprocessed_NoOutputFlag(t *testing.T) {
	c := &clangCompiler{name: "clang"}
	inv, err := ParseGNU([]string{"-c", "/src/foo.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, "/staged/foo.i")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, a := range got.RawArgs {
		if a == "-o" {
			t.Errorf("did not expect -o when input had none, got %v", got.RawArgs)
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

func TestRewriteForPreprocessed_MSVC_NoOutputFlag(t *testing.T) {
	c := &clCompiler{name: "cl"}
	inv, err := ParseMSVC([]string{"/c", "foo.cpp"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := c.RewriteForPreprocessed(inv, `C:\staged\foo.i`)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, a := range got.RawArgs {
		if strings.HasPrefix(a, "/Fo") {
			t.Errorf("did not expect /Fo when input had none, got %v", got.RawArgs)
		}
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
