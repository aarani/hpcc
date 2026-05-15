package compiler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zeebo/blake3"
)

func TestRunPreprocessor_capturesAndHashes(t *testing.T) {
	res, err := runPreprocessor(LocalExecutor{}, "sh", []string{"-c", "printf 'hello world'"}, "")
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
	res, err := runPreprocessor(LocalExecutor{}, "sh", []string{
		"-c", "printf 'partial' ; echo failure-msg >&2 ; exit 7",
	}, "")
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
	_, err := runPreprocessor(LocalExecutor{}, "/no/such/binary/exists", nil, "")
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

// Regression: the kernel build passes `-Wp,-MMD,<file>` (and bare
// `-MD` / `-MMD` / `-MF <file>` etc.) on every gcc invocation. Those
// flags are dep-emission mode flags; if they survive into our `-M`
// invocation in FindDependencies they conflict with `-M` and the
// resulting dep list silently drops `-include`'d headers reached
// through `-isystem` paths (the kernel's
// `include/linux/compiler-version.h` is the canonical case). The
// downstream symptom is a CAS-mode worker compile failing on a
// missing `-include` because the manifest was incomplete.
//
// The stricter variant — used by FindDependencies — must drop the
// dep-emission family. The non-strict variant —
// stripGNUModeAndOutput — must KEEP them, since PREPROCESSED dispatch
// relies on the user's `-Wp,-MMD,foo.d` running as a side-effect of
// the client-side `gcc -E` to keep `make`'s .d files current.
func TestStripGNUModeOutputAndDepEmission_dropsDepEmissionFlags(t *testing.T) {
	in := []string{
		"-c",
		"-Wp,-MMD,scripts/mod/.empty.o.d",
		"-MD", "-MF", "deps.d", "-MT", "target.o", "-MQ", "target.o",
		"-MMD",
		"-Iinclude", "-DDEBUG=1",
		"-Wp,-MD,other.d",
		"-Wp,-MF,kept-because-MF-only",
		"-O2", "foo.c",
	}
	want := []string{
		"-Iinclude", "-DDEBUG=1",
		"-O2", "foo.c",
	}
	got := stripGNUModeOutputAndDepEmission(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// PREPROCESSED dispatch's client-side `gcc -E` depends on
// `-Wp,-MMD,foo.d` (and the rest of the dep-emission family) running
// as a side-effect to keep `make`'s incremental dep tracking up to
// date. stripGNUModeAndOutput must NOT drop them — only the stricter
// stripGNUModeOutputAndDepEmission (used by FindDependencies for the
// internal -M call) does.
func TestStripGNUModeAndOutput_keepsDepEmissionFlags(t *testing.T) {
	in := []string{
		"-c", "-Wp,-MMD,scripts/mod/.empty.o.d",
		"-MD", "-MF", "deps.d",
		"-Iinclude", "-O2", "foo.c",
	}
	want := []string{
		"-Wp,-MMD,scripts/mod/.empty.o.d",
		"-MD", "-MF", "deps.d",
		"-Iinclude", "-O2", "foo.c",
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
