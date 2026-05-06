package compiler

import (
	"strings"

	"github.com/aarani/hpcc/internal/enum"
)

// ParseMSVC parses a cl.exe-style argv into an Invocation. Same Invocation
// shape as ParseGNU; only the grammar differs.
func ParseMSVC(args []string) (*Invocation, error) {
	expanded, err := expandResponseFiles(args, 0)
	if err != nil {
		return nil, err
	}

	inv := NewInvocation()
	inv.RawArgs = expanded

	i := 0
	for i < len(expanded) {
		a := expanded[i]

		if !isMSVCFlag(a) {
			inv.Inputs = append(inv.Inputs, a)
			i++
			continue
		}

		spec, value, consumed, ok := matchMSVCFlag(expanded, i)
		if !ok {
			inv.Unknown = append(inv.Unknown, a)
			i++
			continue
		}

		// /link is a hard divider: everything after goes to the linker
		// verbatim. Don't keep parsing as compiler flags.
		if spec.Name == "/link" {
			inv.Passthrough = append(inv.Passthrough, expanded[i:]...)
			break
		}

		applyMSVCFlag(inv, spec, value)
		i += consumed
	}

	if inv.Mode == enum.UnknownMode {
		inv.Mode = enum.LinkMode
	}
	return inv, nil
}

// isMSVCFlag returns true if arg is shaped like a cl.exe flag (leading "/"
// or "-", and at least one character after).
func isMSVCFlag(arg string) bool {
	return len(arg) > 1 && (arg[0] == '/' || arg[0] == '-')
}

// canonicalizeMSVC normalizes "-flag" to "/flag" so the spec table only
// needs the "/" form. Non-flag args pass through unchanged.
func canonicalizeMSVC(arg string) string {
	if strings.HasPrefix(arg, "-") {
		return "/" + arg[1:]
	}
	return arg
}

func matchMSVCFlag(args []string, i int) (FlagSpec, string, int, bool) {
	canon := canonicalizeMSVC(args[i])
	for _, spec := range msvcFlagsSorted {
		switch spec.Value {
		case NoValue:
			if canon == spec.Name {
				return spec, "", 1, true
			}
		case SeparateValue:
			if canon == spec.Name {
				if i+1 < len(args) {
					return spec, args[i+1], 2, true
				}
				return spec, "", 1, true
			}
		case JoinedValue:
			if strings.HasPrefix(canon, spec.Name) && canon != spec.Name {
				return spec, trimMSVCColon(canon[len(spec.Name):]), 1, true
			}
		case JoinedOrSeparate:
			if canon == spec.Name {
				if i+1 < len(args) {
					return spec, args[i+1], 2, true
				}
				return spec, "", 1, true
			}
			if strings.HasPrefix(canon, spec.Name) {
				return spec, trimMSVCColon(canon[len(spec.Name):]), 1, true
			}
		}
	}
	return FlagSpec{}, "", 0, false
}

// trimMSVCColon strips one leading ":" so /Fo:foo.obj and /Fofoo.obj parse
// to the same value.
func trimMSVCColon(s string) string {
	if strings.HasPrefix(s, ":") {
		return s[1:]
	}
	return s
}

// applyMSVCFlag handles the MSVC-specific flag names that applyFlag's
// switches don't know about (mode names, debug switches, /Tc, /Tp), then
// delegates the rest to applyFlag for generic category handling.
func applyMSVCFlag(inv *Invocation, spec FlagSpec, value string) {
	switch spec.Name {
	case "/c":
		inv.Mode = enum.CompileMode
		return
	case "/E", "/EP", "/P":
		inv.Mode = enum.PreprocessMode
		return
	case "/Zi", "/Z7", "/ZI":
		inv.Debug = spec.Name // record which form so cache keys differ
		return
	case "/Tc":
		inv.Language = "c"
		inv.Inputs = append(inv.Inputs, value)
		return
	case "/Tp":
		inv.Language = "c++"
		inv.Inputs = append(inv.Inputs, value)
		return
	}
	applyFlag(inv, spec, value)
}
