package compiler

import (
	"fmt"
	"os"
	"strings"

	"github.com/aarani/hpcc/internal/enum"
)

// ParseGNU parses a GCC/Clang-style argv into an Invocation.
//
// The parser is lenient: anything that starts with "-" but doesn't match a
// known flag is recorded in Invocation.Unknown so we can re-emit it verbatim.
// Anything that doesn't start with "-" (or "-" alone) is treated as an input
// file.
//
// "@file" response files are expanded recursively before parsing. Tokenization
// inside a response file is whitespace-only — backslash escapes and quoted
// strings are not honored. That covers ~all real-world build systems; if it
// ever bites us we can do proper shell-lexing then.
func ParseGNU(args []string) (*Invocation, error) {
	expanded, err := expandResponseFiles(args, 0)
	if err != nil {
		return nil, err
	}

	inv := NewInvocation()
	inv.RawArgs = expanded

	i := 0
	for i < len(expanded) {
		a := expanded[i]

		// "--" ends flag parsing; everything after is an input.
		if a == "--" {
			inv.Inputs = append(inv.Inputs, expanded[i+1:]...)
			break
		}

		// Bare "-" is conventionally stdin as an input.
		if !strings.HasPrefix(a, "-") || a == "-" {
			inv.Inputs = append(inv.Inputs, a)
			i++
			continue
		}

		spec, value, consumed, ok := matchGNUFlag(expanded, i)
		if !ok {
			inv.Unknown = append(inv.Unknown, a)
			i++
			continue
		}
		applyFlag(inv, spec, value)
		i += consumed
	}

	if inv.Mode == enum.UnknownMode {
		inv.Mode = enum.LinkMode
	}
	return inv, nil
}

// matchGNUFlag tries to match args[i] against gnuFlagsSorted. Returns the
// matched spec, the extracted value (empty if NoValue), how many argv slots
// were consumed (1 or 2), and whether a match was found.
func matchGNUFlag(args []string, i int) (FlagSpec, string, int, bool) {
	arg := args[i]
	for _, spec := range gnuFlagsSorted {
		switch spec.Value {
		case NoValue:
			if arg == spec.Name {
				return spec, "", 1, true
			}
		case SeparateValue:
			if arg == spec.Name {
				if i+1 < len(args) {
					return spec, args[i+1], 2, true
				}
				return spec, "", 1, true // missing value; accept anyway
			}
		case JoinedValue:
			if strings.HasPrefix(arg, spec.Name) {
				return spec, arg[len(spec.Name):], 1, true
			}
		case JoinedOrSeparate:
			if arg == spec.Name {
				if i+1 < len(args) {
					return spec, args[i+1], 2, true
				}
				return spec, "", 1, true
			}
			if strings.HasPrefix(arg, spec.Name) {
				return spec, arg[len(spec.Name):], 1, true
			}
		}
	}
	return FlagSpec{}, "", 0, false
}

func applyFlag(inv *Invocation, spec FlagSpec, value string) {
	switch spec.Category {
	case CatMode:
		switch spec.Name {
		case "-c":
			inv.Mode = enum.CompileMode
		case "-E":
			inv.Mode = enum.PreprocessMode
		case "-S":
			inv.Mode = enum.AssembleMode
		case "-M", "-MM":
			inv.Mode = enum.DepOnlyMode
		}
	case CatOutput:
		inv.Output = value
	case CatInclude:
		inv.Includes = append(inv.Includes, value)
	case CatSystemInclude:
		inv.SystemIncludes = append(inv.SystemIncludes, value)
	case CatDefine:
		name, val, _ := strings.Cut(value, "=")
		inv.Defines[name] = val
	case CatUndefine:
		inv.Undefines = append(inv.Undefines, value)
	case CatLibrary:
		inv.Libraries = append(inv.Libraries, value)
	case CatLibraryDir:
		inv.LibraryDirs = append(inv.LibraryDirs, value)
	case CatStandard:
		inv.Std = value
	case CatOptim:
		inv.Optim = value
	case CatDebug:
		inv.Debug = value
	case CatWarning:
		inv.Warnings = append(inv.Warnings, value)
	case CatFeature:
		inv.Features = append(inv.Features, value)
	case CatMachine:
		inv.Machine = append(inv.Machine, value)
	case CatLanguage:
		inv.Language = value
	case CatPassthrough:
		// Reconstruct the original token form for clean re-emission.
		switch spec.Value {
		case NoValue:
			inv.Passthrough = append(inv.Passthrough, spec.Name)
		case JoinedValue:
			inv.Passthrough = append(inv.Passthrough, spec.Name+value)
		case SeparateValue, JoinedOrSeparate:
			inv.Passthrough = append(inv.Passthrough, spec.Name, value)
		}
	}
}

// expandResponseFiles expands "@file" args recursively. Depth-limited to
// catch accidental cycles.
func expandResponseFiles(args []string, depth int) ([]string, error) {
	if depth > 16 {
		return nil, fmt.Errorf("response file recursion too deep")
	}
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "@") || len(a) == 1 {
			out = append(out, a)
			continue
		}
		path := a[1:]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read response file %q: %w", path, err)
		}
		expanded, err := expandResponseFiles(strings.Fields(string(data)), depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}
	return out, nil
}
