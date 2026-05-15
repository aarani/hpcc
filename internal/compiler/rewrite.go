package compiler

import (
	"fmt"
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
	"/home/",  // Linux user homes
	"/Users/", // macOS user homes
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
			out = append(out, args[i:i+consumed]...)
		}
		i += consumed
	}
	return out
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
