package compiler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zeebo/blake3"
)

func TestRunPreprocessor_capturesAndHashes(t *testing.T) {
	res, err := runPreprocessor("sh", []string{"-c", "printf 'hello world'"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Source) != "hello world" {
		t.Errorf("Source = %q, want %q", res.Source, "hello world")
	}
	want := blake3.Sum256([]byte("hello world"))
	if res.Digest != want {
		t.Errorf("Digest mismatch")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestRunPreprocessor_capturesStderrAndExitCode(t *testing.T) {
	res, err := runPreprocessor("sh", []string{
		"-c", "printf 'partial' ; echo failure-msg >&2 ; exit 7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "failure-msg") {
		t.Errorf("Stderr = %q, missing failure-msg", res.Stderr)
	}
	// Source captured up to the failure point, and digest reflects it.
	if string(res.Source) != "partial" {
		t.Errorf("Source = %q, want partial", res.Source)
	}
}

func TestRunPreprocessor_unrunnable(t *testing.T) {
	_, err := runPreprocessor("/no/such/binary/exists", nil)
	if err == nil {
		t.Errorf("expected error for missing binary, got nil")
	}
}

func TestStripGNUModeAndOutput(t *testing.T) {
	in := []string{
		"-c", "-Iinclude", "-isystem", "/sys/inc",
		"-DDEBUG=1", "-o", "foo.o", "-O2", "foo.c",
	}
	want := []string{
		"-Iinclude", "-isystem", "/sys/inc",
		"-DDEBUG=1", "-O2", "foo.c",
	}
	got := stripGNUModeAndOutput(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestParseMakeDeps(t *testing.T) {
	in := []byte("foo.o: foo.c bar.h \\\n" +
		"  /usr/include/stdio.h \\\n" +
		"  /usr/include/features.h\n")
	want := []string{
		"foo.c", "bar.h",
		"/usr/include/stdio.h",
		"/usr/include/features.h",
	}
	got := parseMakeDeps(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestParseMakeDeps_singleLine(t *testing.T) {
	got := parseMakeDeps([]byte("foo.o: foo.c bar.h baz.h\n"))
	want := []string{"foo.c", "bar.h", "baz.h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestParseShowIncludes(t *testing.T) {
	in := []byte(
		"foo.cpp\r\n" +
			"Note: including file: C:\\inc\\foo.h\r\n" +
			"Note: including file:  C:\\inc\\nested.h\r\n" +
			"Note: including file:   C:\\inc\\deeper.h\r\n" +
			"Some other line of compiler output\r\n",
	)
	got := parseShowIncludes(in)
	want := []string{
		`C:\inc\foo.h`,
		`C:\inc\nested.h`,
		`C:\inc\deeper.h`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestFilterSources(t *testing.T) {
	deps := []string{"foo.c", "bar.h", "baz.h"}
	got := filterSources(deps, []string{"foo.c"})
	want := []string{"bar.h", "baz.h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestFilterSources_noopWhenNoSources(t *testing.T) {
	deps := []string{"a.h", "b.h"}
	got := filterSources(deps, nil)
	if !reflect.DeepEqual(got, deps) {
		t.Errorf("got %v want %v", got, deps)
	}
}

func TestStripMSVCModeAndOutput(t *testing.T) {
	in := []string{
		"/c", "/Iinclude", "/Fo:foo.obj", "/std:c++20",
		"/W4", "foo.cpp",
	}
	want := []string{
		"/Iinclude", "/std:c++20", "/W4", "foo.cpp",
	}
	got := stripMSVCModeAndOutput(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
