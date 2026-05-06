package compiler

import "sort"

// ValueMode describes how a flag carries its value (if any).
type ValueMode int

const (
	NoValue          ValueMode = iota // -c, -E (pure switch)
	JoinedValue                       // -O2, -Wall, -std=c++17 (value glued on)
	SeparateValue                     // -o foo, -isystem /path (value is next arg)
	JoinedOrSeparate                  // -I (accepts either form)
)

// Category is what kind of flag this is, semantically. The parser uses it to
// decide which Invocation field to populate.
type Category int

const (
	CatUnknown Category = iota
	CatMode             // -c, -E, -S, -M, -MM
	CatOutput           // -o
	CatInclude          // -I
	CatSystemInclude    // -isystem, -iquote
	CatDefine           // -D
	CatUndefine         // -U
	CatLibrary          // -l
	CatLibraryDir       // -L
	CatStandard         // -std=
	CatOptim            // -O
	CatDebug            // -g
	CatWarning          // -W
	CatFeature          // -f
	CatMachine          // -m
	CatLanguage         // -x
	CatPassthrough      // -Wl,..., -Xlinker, -include, -MD, etc.
)

type FlagSpec struct {
	Name     string
	Value    ValueMode
	Category Category
}

// gnuFlags is the GNU/Clang argument grammar. Order doesn't matter here —
// the package init sorts by name length descending so longer prefixes
// (e.g. "-Wl,") win over shorter ones ("-W") during matching.
var gnuFlags = []FlagSpec{
	// Modes
	{"-c", NoValue, CatMode},
	{"-E", NoValue, CatMode},
	{"-S", NoValue, CatMode},
	{"-M", NoValue, CatMode},
	{"-MM", NoValue, CatMode},

	// Output
	{"-o", SeparateValue, CatOutput},

	// Includes
	{"-I", JoinedOrSeparate, CatInclude},
	{"-isystem", SeparateValue, CatSystemInclude},
	{"-iquote", SeparateValue, CatSystemInclude},

	// Macros
	{"-D", JoinedOrSeparate, CatDefine},
	{"-U", JoinedOrSeparate, CatUndefine},

	// Linker inputs
	{"-l", JoinedOrSeparate, CatLibrary},
	{"-L", JoinedOrSeparate, CatLibraryDir},

	// Language / standard
	{"-std=", JoinedValue, CatStandard},
	{"-x", SeparateValue, CatLanguage},

	// Code-gen knobs
	{"-O", JoinedValue, CatOptim},
	{"-g", JoinedValue, CatDebug},
	{"-W", JoinedValue, CatWarning},
	{"-f", JoinedValue, CatFeature},
	{"-m", JoinedValue, CatMachine},

	// Passthrough — recognized so we don't drop them into Unknown, but
	// nothing structured to do beyond re-emitting them verbatim.
	{"-Wl,", JoinedValue, CatPassthrough},
	{"-Wa,", JoinedValue, CatPassthrough},
	{"-Wp,", JoinedValue, CatPassthrough},
	{"-Xlinker", SeparateValue, CatPassthrough},
	{"-Xassembler", SeparateValue, CatPassthrough},
	{"-Xpreprocessor", SeparateValue, CatPassthrough},
	{"-include", SeparateValue, CatPassthrough},
	{"-MD", NoValue, CatPassthrough},
	{"-MMD", NoValue, CatPassthrough},
	{"-MF", SeparateValue, CatPassthrough},
	{"-MT", SeparateValue, CatPassthrough},
	{"-MQ", SeparateValue, CatPassthrough},
}

// gnuFlagsSorted is gnuFlags ordered by Name length descending. The matcher
// scans this in order and takes the first hit, so e.g. "-Wl," matches before
// "-W" for an arg like "-Wl,--as-needed".
var gnuFlagsSorted []FlagSpec

func init() {
	gnuFlagsSorted = append(gnuFlagsSorted, gnuFlags...)
	sort.SliceStable(gnuFlagsSorted, func(i, j int) bool {
		return len(gnuFlagsSorted[i].Name) > len(gnuFlagsSorted[j].Name)
	})
}
