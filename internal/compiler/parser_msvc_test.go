package compiler

import (
	"reflect"
	"testing"

	"github.com/aarani/hpcc/internal/enum"
)

func TestParseMSVC_basicCompile(t *testing.T) {
	inv, err := ParseMSVC([]string{"/c", "/Fo:foo.obj", "foo.cpp"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Mode != enum.CompileMode {
		t.Errorf("Mode = %v, want CompileMode", inv.Mode)
	}
	if inv.Output != "foo.obj" {
		t.Errorf("Output = %q, want foo.obj", inv.Output)
	}
	if !reflect.DeepEqual(inv.Inputs, []string{"foo.cpp"}) {
		t.Errorf("Inputs = %v", inv.Inputs)
	}
}

func TestParseMSVC_dashFormEquivalent(t *testing.T) {
	a, _ := ParseMSVC([]string{"/c", "/Fo:out.obj", "x.cpp"})
	b, _ := ParseMSVC([]string{"-c", "-Fo:out.obj", "x.cpp"})
	if a.Mode != b.Mode || a.Output != b.Output ||
		!reflect.DeepEqual(a.Inputs, b.Inputs) {
		t.Errorf("dash form should equal slash form: a=%+v b=%+v", a, b)
	}
}

func TestParseMSVC_colonOptional(t *testing.T) {
	withColon, _ := ParseMSVC([]string{"/Fo:foo.obj"})
	noColon, _ := ParseMSVC([]string{"/Fofoo.obj"})
	if withColon.Output != "foo.obj" || noColon.Output != "foo.obj" {
		t.Errorf("colon should be optional: with=%q no=%q",
			withColon.Output, noColon.Output)
	}
}

func TestParseMSVC_stdAndDefines(t *testing.T) {
	inv, _ := ParseMSVC([]string{"/std:c++20", "/DDEBUG=1", "/DFOO"})
	if inv.Std != "c++20" {
		t.Errorf("Std = %q", inv.Std)
	}
	if inv.Defines["DEBUG"] != "1" || inv.Defines["FOO"] != "" {
		t.Errorf("Defines = %v", inv.Defines)
	}
}

func TestParseMSVC_longestPrefixWins(t *testing.T) {
	// /MDd (4 chars) must match before /MD (3 chars).
	inv, _ := ParseMSVC([]string{"/MDd", "foo.cpp"})
	if !reflect.DeepEqual(inv.Passthrough, []string{"/MDd"}) {
		t.Errorf("Passthrough = %v, want [/MDd]", inv.Passthrough)
	}
	inv, _ = ParseMSVC([]string{"/MD", "foo.cpp"})
	if !reflect.DeepEqual(inv.Passthrough, []string{"/MD"}) {
		t.Errorf("Passthrough = %v, want [/MD]", inv.Passthrough)
	}
}

func TestParseMSVC_warningsBucket(t *testing.T) {
	inv, _ := ParseMSVC([]string{"/W4", "/Wall", "/WX"})
	want := []string{"4", "all", "X"}
	if !reflect.DeepEqual(inv.Warnings, want) {
		t.Errorf("Warnings = %v, want %v", inv.Warnings, want)
	}
}

func TestParseMSVC_includesBothForms(t *testing.T) {
	inv, _ := ParseMSVC([]string{"/Iinc1", "/I", "inc2"})
	if !reflect.DeepEqual(inv.Includes, []string{"inc1", "inc2"}) {
		t.Errorf("Includes = %v", inv.Includes)
	}
}

func TestParseMSVC_TcAddsToInputs(t *testing.T) {
	inv, _ := ParseMSVC([]string{"/c", "/Tc", "weird.txt"})
	if !reflect.DeepEqual(inv.Inputs, []string{"weird.txt"}) {
		t.Errorf("Inputs = %v, want [weird.txt]", inv.Inputs)
	}
	if inv.Language != "c" {
		t.Errorf("Language = %q, want c", inv.Language)
	}
}

func TestParseMSVC_linkHaltsParsing(t *testing.T) {
	inv, _ := ParseMSVC([]string{
		"/c", "foo.cpp", "/link", "/LIBPATH:lib", "/SUBSYSTEM:CONSOLE",
	})
	want := []string{"/link", "/LIBPATH:lib", "/SUBSYSTEM:CONSOLE"}
	if !reflect.DeepEqual(inv.Passthrough, want) {
		t.Errorf("Passthrough = %v, want %v", inv.Passthrough, want)
	}
	if inv.Mode != enum.CompileMode {
		t.Errorf("Mode = %v, want CompileMode (set before /link)", inv.Mode)
	}
}

func TestParseMSVC_unknownPassesThrough(t *testing.T) {
	inv, _ := ParseMSVC([]string{"/notArealFlag", "foo.cpp"})
	if !reflect.DeepEqual(inv.Unknown, []string{"/notArealFlag"}) {
		t.Errorf("Unknown = %v", inv.Unknown)
	}
}

func TestParseMSVC_defaultsToLink(t *testing.T) {
	inv, _ := ParseMSVC([]string{"foo.obj", "bar.obj", "/Fe:prog.exe"})
	if inv.Mode != enum.LinkMode {
		t.Errorf("Mode = %v, want LinkMode", inv.Mode)
	}
	if inv.Output != "prog.exe" {
		t.Errorf("Output = %q", inv.Output)
	}
}
