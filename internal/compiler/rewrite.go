package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aarani/hpcc/internal/enum"
)

// hostPathSubstrings is the set of substrings that are virtually never
// legitimate inside a per-tenant compile sandbox: developer-machine home
// directories on Linux, macOS, and Windows. If any of these appears in
// argv after client-side rewriting, the client failed to do its job —
// surface it loudly here rather than letting the compile silently fail
// inside the VM with a confusing "no such file or directory."
//
// Deliberately conservative — only paths that *originate* on a dev
// machine. System dirs like /usr, /opt, /tmp, /var live both on the host
// and inside the toolchain image and are intentionally not flagged.
var hostPathSubstrings = []string{
	"/home/",   // Linux user homes
	"/Users/",  // macOS user homes
	`:\Users\`, // Windows user homes (any drive letter)
	`:\users\`, // Windows user homes, lowercase variant
}

// ValidateNoHostPaths returns an error if any argv element contains a
// substring that looks like a developer-machine path. Intended to run
// on the worker side as a sanity check after the client should have
// rewritten paths via RewritePathPrefix or RewriteForPreprocessed.
//
// Catches the common failure modes:
//   - Client forgot to call the rewrite for the chosen source mode.
//   - Client rewrote some paths but missed flags (e.g. -isystem on the
//     separate-value form).
//   - Misconfigured client sent raw argv straight from a CMake build dir.
//
// False-positive case: a user truly has a system dir literally named
// "Users" (very rare on Linux) — they can't run hpcc anyway. False
// negatives: paths under /opt, /var/ci-runner, /scratch — accepted on
// the assumption that those rarely originate on the dev side.
func ValidateNoHostPaths(args []string) error {
	for _, a := range args {
		for _, p := range hostPathSubstrings {
			if strings.Contains(a, p) {
				return fmt.Errorf("argv contains host-shaped path (substring %q in %q); the client must rewrite paths for the chosen source mode before sending",
					p, a)
			}
		}
	}
	return nil
}

// RewritePathPrefix returns a shallow copy of inv with every host-side
// path prefix replaced by the in-container prefix. Used client-side
// when shipping a CAS-mode CompileRequest: argv references like
// `-I/home/alice/proj/include` become `-I/src/include` (with
// hostPrefix = "/home/alice/proj", vmPrefix = "/src").
//
// The substitution is a substring replace on each argv entry, not a
// flag-aware rewrite — so it handles joined forms (-I<path>), separate
// forms (-isystem <path>), and bare positional inputs uniformly. The
// trade-off is false positives in the rare case where a non-path flag
// value happens to contain hostPrefix (e.g. -DPATH=/home/alice/proj/x).
// Callers should keep their hostPrefix specific enough that a stray
// match is unlikely.
//
// Trailing slashes on either prefix are normalized away. An exact
// boundary match (host path with no trailing component) also rewrites,
// so a positional input that is itself the project root maps cleanly.
func RewritePathPrefix(inv *Invocation, hostPrefix, vmPrefix string) *Invocation {
	host := strings.TrimRight(filepath.ToSlash(hostPrefix), "/")
	vm := strings.TrimRight(filepath.ToSlash(vmPrefix), "/")
	if host == "" || vm == "" {
		return inv
	}

	rewrite := func(s string) string {
		return rewritePrefix(s, host, vm)
	}

	rewriteSlice := func(in []string) []string {
		if in == nil {
			return nil
		}
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = rewrite(s)
		}
		return out
	}

	cp := *inv
	cp.RawArgs = rewriteSlice(inv.RawArgs)
	cp.Inputs = rewriteSlice(inv.Inputs)
	cp.Includes = rewriteSlice(inv.Includes)
	cp.SystemIncludes = rewriteSlice(inv.SystemIncludes)
	cp.LibraryDirs = rewriteSlice(inv.LibraryDirs)
	cp.Output = rewrite(inv.Output)
	if inv.Defines != nil {
		cp.Defines = make(map[string]string, len(inv.Defines))
		for k, v := range inv.Defines {
			cp.Defines[k] = rewrite(v)
		}
	}
	return &cp
}

// RewriteForCAS prepares an Invocation for CAS-mode dispatch. It rewrites:
//
//   - All paths matching projectRoot to live under srcRoot ("/src" inside
//     the container). System paths (e.g. /usr/include/...) stay absolute.
//   - The output to outRoot + "/" + basename(output) ("/out/<base>"), so
//     the worker can collect the artifact from a known location. The -o
//     flag in RawArgs is patched in-place to match.
//
// Unlike RewriteForPreprocessed, this does NOT drop -I/-D — CAS-mode
// compiles still run the preprocessor on the worker side, so include
// paths and macro defines have to ride along. The path rewrite handles
// the in-container locations.
//
// projectRoot must be absolute and stripped of any trailing slash; pass
// it as returned by FindProjectRoot. srcRoot and outRoot are the
// in-container mount points (typically "/src" and "/out").
func RewriteForCAS(inv *Invocation, projectRoot, srcRoot, outRoot string) *Invocation {
	cp := RewritePathPrefix(inv, projectRoot, srcRoot)
	if inv.Output == "" {
		return cp
	}
	newOut := outRoot + "/" + filepath.Base(inv.Output)
	cp.Output = newOut
	cp.RawArgs = rewriteOutputFlagGNU(cp.RawArgs, newOut)
	return cp
}

// ExtractDepEmissionPaths returns the list of dep-file output paths
// the argv asks the compiler to write — i.e. the `<path>` in any
// `-Wp,-MMD,<path>` / `-Wp,-MD,<path>` / `-Wp,-MF,<path>` form, plus
// the separate `-MF <path>` form. The caller treats these as
// expected side-effect outputs to capture after running the
// compiler (PREPROCESSED dispatch) or expects them back from the
// worker (CAS, via RewriteDepEmissionForCAS).
//
// Pure read; doesn't modify args. Paths are returned in argv order
// and as-spelled (no normalization), so the caller can use them as
// keys/paths against the same cwd the compiler ran in.
func ExtractDepEmissionPaths(args []string) []string {
	out := make([]string, 0)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if _, p, ok := splitWpDepEmission(a); ok {
			out = append(out, p)
			continue
		}
		if a == "-MF" && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
			continue
		}
	}
	return out
}

// CollectDepEmissionExtras reads the dep-emission output files the
// compiler just produced (one per path returned by
// ExtractDepEmissionPaths) off disk, returning them keyed by their
// as-spelled argv path. Used by every callsite that runs the user's
// compiler with `-Wp,-MMD,…` / `-MF` still in argv (i.e. expects the
// .d files on disk afterwards) and wants to round-trip them through
// the cache so warm hits replay the same .d files the cold compile
// produced.
//
// Returns nil if argv carries no dep-emission flags. Returns an
// (otherwise non-nil) map even when individual files are missing —
// a flag like `-Wp,-MMD,<path>` may produce no output for a source
// with no #includes; that's not an error, just nothing to cache for
// that entry.
//
// Relative paths are resolved against inv.Cwd. Read errors other
// than ENOENT are silently skipped on the same theory: an extras
// blob is best-effort, the primary artifact is the contract.
func CollectDepEmissionExtras(inv *Invocation) map[string][]byte {
	depPaths := ExtractDepEmissionPaths(inv.RawArgs)
	if len(depPaths) == 0 {
		return nil
	}
	extras := make(map[string][]byte, len(depPaths))
	for _, p := range depPaths {
		full := p
		if !filepath.IsAbs(p) && inv.Cwd != "" {
			full = filepath.Join(inv.Cwd, p)
		}
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		extras[p] = b
	}
	if len(extras) == 0 {
		return nil
	}
	return extras
}

// splitWpDepEmission parses a -Wp,-M*,PATH flag into (prefix, path).
// Mirrors rewriteWpDepEmission's prefix table; refactored out so
// extraction and rewriting share the same set of recognised forms.
func splitWpDepEmission(a string) (prefix, path string, ok bool) {
	for _, pfx := range []string{"-Wp,-MMD,", "-Wp,-MD,", "-Wp,-MF,"} {
		if strings.HasPrefix(a, pfx) {
			return pfx, a[len(pfx):], true
		}
	}
	return "", "", false
}

// RewriteDepEmissionForCAS rewrites every dep-emission path in args to
// live under outRoot inside the container, returning the new argv and
// the list of /out-relative paths the worker will produce. The client
// uses the second return to know which side-effect files to expect
// back in CompileResponse.extra_outputs and where to write them on
// the host (joined against inv.Cwd, which has the same relative
// structure).
//
// Rewrites two forms:
//
//   - `-Wp,-MMD,<path>` and the rest of the `-Wp,-M*,<path>` family
//     (the kernel build uses these on every TU).
//   - Separate-form `-MF <path>` (the standalone version of the same).
//
// Bare `-MD` / `-MMD` (no explicit path) is intentionally not handled —
// gcc derives the default path from `-o`, and the rules are
// platform-specific (`<output_basename>.d` in the same dir). Easy to
// add when something asks for it.
func RewriteDepEmissionForCAS(args []string, outRoot string) (newArgs []string, paths []string) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if rewritten, p, ok := rewriteWpDepEmission(a, outRoot); ok {
			out = append(out, rewritten)
			paths = append(paths, p)
			continue
		}
		if (a == "-MF" || a == "-MT" || a == "-MQ") && i+1 < len(args) {
			// -MF specifies the dep-file output path; rewrite its
			// value. -MT/-MQ name the *target* in the .d rule, not
			// an output path, so leave them alone.
			if a == "-MF" {
				orig := args[i+1]
				out = append(out, a, filepath.ToSlash(filepath.Join(outRoot, orig)))
				paths = append(paths, orig)
				i++
				continue
			}
			out = append(out, a, args[i+1])
			i++
			continue
		}
		out = append(out, a)
	}
	return out, paths
}

// rewriteWpDepEmission rewrites a -Wp,-M*,PATH flag to point under
// outRoot, returning (newFlag, originalPath, true) on a match.
func rewriteWpDepEmission(a, outRoot string) (string, string, bool) {
	prefix, orig, ok := splitWpDepEmission(a)
	if !ok {
		return "", "", false
	}
	return prefix + filepath.ToSlash(filepath.Join(outRoot, orig)), orig, true
}

// rewriteOutputFlagGNU walks args looking for `-o <value>` or `-o<value>`
// and replaces the value. Returns a new slice; does not mutate the input.
// Handles both the separate form (-o foo.o, two argv slots) and the
// joined form (-ofoo.o, one slot). MSVC's /Fo: equivalent is a follow-up
// when we add MSVC support to the CAS path.
func rewriteOutputFlagGNU(args []string, newOut string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-o" && i+1 < len(args) {
			out = append(out, "-o", newOut)
			i++
			continue
		}
		if strings.HasPrefix(a, "-o") && a != "-o" {
			out = append(out, "-o"+newOut)
			continue
		}
		out = append(out, a)
	}
	return out
}

// rewritePrefix replaces every occurrence of host in s with vm, but only
// where host sits on a path boundary — i.e. followed by "/" or the end
// of the string. This is what stops "/home/alice/proj-other" from being
// rewritten when host is "/home/alice/proj". One scan, no regex.
func rewritePrefix(s, host, vm string) string {
	if !strings.Contains(s, host) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], host) {
			end := i + len(host)
			if end == len(s) || s[end] == '/' {
				b.WriteString(vm)
				i = end
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// rewriteGNUForPreprocessed builds the argv for compiling preprocessed
// source at srcPath: -x <lang> -c <srcPath> [-o <out>] plus the subset
// of original flags that still apply (code-gen, optimization, language
// standard, warnings, target). Everything preprocess-related (-I, -D,
// -include, -M*) and link-related (-L, -l, -Wl,) is dropped.
func rewriteGNUForPreprocessed(inv *Invocation, srcPath, lang string) *Invocation {
	args := []string{"-x", lang, "-c", srcPath}
	if inv.Output != "" {
		args = append(args, "-o", inv.Output)
	}
	args = append(args, gnuKeepForPreprocessed(inv.RawArgs)...)

	out := &Invocation{
		Mode:     enum.CompileMode,
		Inputs:   []string{srcPath},
		Output:   inv.Output,
		Language: lang,
		RawArgs:  args,
		Defines:  map[string]string{},
	}
	// Preserve code-gen-shaped fields so later code that reads structured
	// state (CacheKey flag-mixing, audit) sees the same values it would
	// if it re-parsed the new RawArgs.
	out.Std = inv.Std
	out.Optim = inv.Optim
	out.Debug = inv.Debug
	out.Warnings = append([]string(nil), inv.Warnings...)
	out.Features = append([]string(nil), inv.Features...)
	out.Machine = append([]string(nil), inv.Machine...)
	out.Unknown = append([]string(nil), inv.Unknown...)
	return out
}

// gnuKeepForPreprocessed returns the subset of args that should survive
// into a preprocessed-source compile: code-gen knobs (-O, -g, -W, -f,
// -m), language standard (-std=), and unrecognized flags (kept on the
// theory that we don't know what they do, so don't drop them). Mode,
// output, includes, defines, language, libraries, and passthrough are
// all dropped — caller re-emits its own mode/output/language.
//
// One special exception: `-Werror` and `-Werror=<class>` are dropped.
// hpcc's preprocessed-mode dispatch is a two-step compile by
// construction (client `gcc -E`, worker `gcc -x cpp-output -c`), and
// gcc's "suppress this diagnostic when the token sits inside a macro
// expansion" heuristic — which protects many warning classes
// (`-Wtautological-compare`, `-Waddress`, `-Wstring-compare`,
// `-Wsizeof-pointer-div`, …) — depends on the in-memory macro-
// expansion table the preprocessor builds. Once -E writes the
// expanded text to disk and a separate cc1 reads it back, the table
// is gone, the suppression rule can't fire, and warnings surface on
// what would have been clean code in one-step mode. Keeping the
// user's `-Werror` promotions across that seam would turn every such
// warning into a build failure on macro-heavy codebases (the Linux
// kernel is the obvious example: BUILD_BUG_ON_ZERO / __same_type
// expansions trigger -Wtautological-compare at column-1100+
// positions, deep in the expanded line).
//
// The compile still emits the warnings — they're visible in build
// logs — they just don't fail the build. Codegen is unchanged. This
// makes the FC compile match what local-mode gcc one-step would have
// produced. The dispatcher prints a one-time explanation to the user
// on the first remote compile so the demotion is auditable, not
// silent. Long-term, CAS-mode dispatch (plan §4.5) ships the source
// tree itself and lets the worker do a real one-step compile,
// sidestepping the issue entirely.
func gnuKeepForPreprocessed(args []string) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			// Everything past "--" is a positional input; we provide srcPath ourselves.
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			// Bare positional input — drop, replaced by srcPath.
			i++
			continue
		}
		spec, _, consumed, ok := matchGNUFlag(args, i)
		if !ok {
			// Unrecognized flag — keep on the conservative side.
			out = append(out, a)
			i++
			continue
		}
		switch spec.Category {
		case CatStandard, CatOptim, CatDebug, CatWarning, CatFeature, CatMachine:
			if spec.Category == CatWarning && isWerrorFlag(a) {
				// Drop — see long comment above.
				break
			}
			out = append(out, args[i:i+consumed]...)
		}
		i += consumed
	}
	return out
}

// isWerrorFlag reports whether arg is a GNU `-Werror` or
// `-Werror=<class>` flag. These get stripped at the rewrite seam
// (see gnuKeepForPreprocessed) because hpcc's preprocessed-mode
// dispatch can't reliably honor them — see the long comment there.
func isWerrorFlag(arg string) bool {
	return arg == "-Werror" || strings.HasPrefix(arg, "-Werror=")
}

// gnuPreprocessedLanguage picks the -x value for the post-preprocess
// compile. Three families:
//
//   - C++ source ("c++-cpp-output"): the compiler is a C++ driver
//     (clang++, g++, c++), the invocation forced -x c++, or the input
//     has a C++-flavored extension.
//   - Assembly ("assembler"): the invocation forced -x assembler /
//     -x assembler-with-cpp, or the input is .s/.S. Once we've run
//     -E ourselves the cpp pass is already done; "assembler" tells
//     gcc to skip it on the second pass. The Linux kernel build is
//     full of .S files (arch/x86/boot/startup/efi-mixed.S,
//     usr/initramfs_data.S, …); without this branch the rewriter
//     mislabels them as "cpp-output" and gcc tries to parse the
//     assembly as C, producing 200-line cascades of "invalid suffix
//     'b' on integer constant" / "stray '\' in program" / "expected
//     identifier or '(' before '.' token".
//   - C source ("cpp-output"): the default for everything else.
func gnuPreprocessedLanguage(compilerName string, inv *Invocation) string {
	if compilerName == "clang++" || compilerName == "g++" || compilerName == "c++" || inv.Language == "c++" {
		return "c++-cpp-output"
	}
	if inv.Language == "assembler" || inv.Language == "assembler-with-cpp" {
		return "assembler"
	}
	for _, in := range inv.Inputs {
		switch strings.ToLower(filepath.Ext(in)) {
		case ".cpp", ".cxx", ".cc", ".c++", ".cp", ".hpp", ".hxx":
			return "c++-cpp-output"
		case ".s", ".S":
			return "assembler"
		}
	}
	return "cpp-output"
}

// rewriteMSVCForPreprocessed builds the argv for compiling preprocessed
// source at srcPath with cl.exe: /c /nologo /Tc<srcPath> (or /Tp for C++)
// /Fo:<output> plus the subset of original flags that still apply
// (code-gen, optimization, language standard, runtime/EH selection,
// debug info). Everything preprocess-related (/I, /D, /U, /external:I,
// /showIncludes), all output overrides (/Fo, /Fe, /Fd, /Fa) and the
// /link tail are dropped — caller re-emits its own mode/output/language.
//
// langFlag is "/Tc" (C) or "/Tp" (C++); the joined form
// "/Tc<srcPath>" forces cl.exe to treat that file as the chosen
// language regardless of extension, which matters because the staged
// preprocessed file is conventionally named main.i.
func rewriteMSVCForPreprocessed(inv *Invocation, srcPath, langFlag string) *Invocation {
	args := []string{"/c", "/nologo", langFlag + srcPath}
	if inv.Output != "" {
		args = append(args, "/Fo:"+inv.Output)
	}
	args = append(args, msvcKeepForPreprocessed(inv.RawArgs)...)

	lang := "c"
	if langFlag == "/Tp" {
		lang = "c++"
	}
	out := &Invocation{
		Mode:     enum.CompileMode,
		Inputs:   []string{srcPath},
		Output:   inv.Output,
		Language: lang,
		RawArgs:  args,
		Defines:  map[string]string{},
	}
	out.Std = inv.Std
	out.Optim = inv.Optim
	out.Debug = inv.Debug
	out.Warnings = append([]string(nil), inv.Warnings...)
	out.Features = append([]string(nil), inv.Features...)
	out.Machine = append([]string(nil), inv.Machine...)
	out.Unknown = append([]string(nil), inv.Unknown...)
	return out
}

// msvcKeepForPreprocessed returns the subset of cl.exe args that should
// survive into a preprocessed-source compile.
//
// Drop policy:
//   - mode (/c, /E, /EP, /P): caller emits /c.
//   - output overrides (/Fo, /Fe): caller emits /Fo:.
//   - preprocessing (/I, /external:I, /D, /U, /showIncludes): irrelevant
//     to a preprocessed-source compile; preprocessing already happened.
//   - language forcing (/Tc, /Tp): caller emits the right one for srcPath.
//   - banner / secondary outputs (/nologo, /Fd PDB, /Fa asm listing):
//     caller adds /nologo; PDB and asm aren't returned over the wire.
//   - /link and everything after: linker passthrough.
//
// Keep policy:
//   - code-gen (/O, /W, /Zi, /Z7, /ZI, /std).
//   - runtime/EH (/MD, /MDd, /MT, /MTd, /EH): grammar marks these
//     CatPassthrough but they materially change the emitted object,
//     so they have to ride along.
//   - unknown flags: kept on the conservative side. New cl.exe versions
//     add flags constantly and a future code-gen knob we don't recognize
//     yet should not silently disappear.
func msvcKeepForPreprocessed(args []string) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		if !isMSVCFlag(a) {
			// Bare positional — drop, replaced by srcPath.
			i++
			continue
		}
		spec, _, consumed, ok := matchMSVCFlag(args, i)
		if !ok {
			out = append(out, a)
			i++
			continue
		}
		if spec.Name == "/link" {
			break
		}

		switch spec.Category {
		case CatStandard, CatOptim, CatDebug, CatWarning, CatFeature, CatMachine:
			out = append(out, args[i:i+consumed]...)
		case CatPassthrough:
			// /MD, /MDd, /MT, /MTd, /EH affect code-gen and must survive.
			// /nologo, /showIncludes, /Fd, /Fa are noise for a
			// preprocessed-source compile — drop.
			switch spec.Name {
			case "/MD", "/MDd", "/MT", "/MTd", "/EH":
				out = append(out, args[i:i+consumed]...)
			}
		}
		i += consumed
	}
	return out
}

// msvcPreprocessedLanguage picks "/Tc" (C) or "/Tp" (C++) for the
// preprocessed-source compile. An explicit /Tc/Tp on the input
// invocation wins; otherwise we look at input file extensions; falling
// back to /Tc (cl.exe's default for ambiguous extensions, including .i).
func msvcPreprocessedLanguage(inv *Invocation) string {
	if inv.Language == "c++" {
		return "/Tp"
	}
	if inv.Language == "c" {
		return "/Tc"
	}
	for _, in := range inv.Inputs {
		switch strings.ToLower(filepath.Ext(in)) {
		case ".cpp", ".cxx", ".cc", ".c++", ".cp", ".hpp", ".hxx", ".ii":
			return "/Tp"
		}
	}
	return "/Tc"
}
