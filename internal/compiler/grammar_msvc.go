package compiler

import "sort"

// msvcFlags is the cl.exe argument grammar.
//
// Two MSVC quirks worth knowing:
//   - flags can start with "/" or "-" (cl.exe accepts both); the parser
//     canonicalizes "-" to "/" before lookup.
//   - many joined flags accept an optional ":" separator: "/Fo:foo.obj" and
//     "/Fofoo.obj" are equivalent. The matcher strips one leading ":" from
//     joined values.
var msvcFlags = []FlagSpec{
	// Modes
	{"/c", NoValue, CatMode},      // compile only
	{"/E", NoValue, CatMode},      // preprocess to stdout
	{"/EP", NoValue, CatMode},     // preprocess to stdout, no #line
	{"/P", NoValue, CatMode},      // preprocess to file

	// Output
	{"/Fo", JoinedValue, CatOutput}, // object file
	{"/Fe", JoinedValue, CatOutput}, // executable

	// Includes / defines
	{"/I", JoinedOrSeparate, CatInclude},
	{"/external:I", SeparateValue, CatSystemInclude}, // /external:I <path>
	{"/D", JoinedOrSeparate, CatDefine},
	{"/U", JoinedOrSeparate, CatUndefine},

	// Standard / language
	{"/std", JoinedValue, CatStandard},     // /std:c++20
	{"/Tc", JoinedOrSeparate, CatLanguage}, // treat next file as C
	{"/Tp", JoinedOrSeparate, CatLanguage}, // treat next file as C++

	// Code-gen
	{"/O", JoinedValue, CatOptim},   // /O1 /O2 /Od /Ox /Os /Ot
	{"/W", JoinedValue, CatWarning}, // /W0../W4, /Wall, /WX
	{"/Zi", NoValue, CatDebug},
	{"/Z7", NoValue, CatDebug},
	{"/ZI", NoValue, CatDebug},

	// Runtime / EH
	{"/MDd", NoValue, CatPassthrough},
	{"/MTd", NoValue, CatPassthrough},
	{"/MD", NoValue, CatPassthrough},
	{"/MT", NoValue, CatPassthrough},
	{"/EH", JoinedValue, CatPassthrough}, // /EHsc /EHa /EHs

	// Sundry passthrough
	{"/nologo", NoValue, CatPassthrough},
	{"/showIncludes", NoValue, CatPassthrough},
	{"/Fd", JoinedValue, CatPassthrough}, // pdb path
	{"/Fa", JoinedValue, CatPassthrough}, // asm listing

	// /link halts compiler-flag parsing; everything after is linker
	// passthrough. Handled specially in ParseMSVC (not via applyFlag).
	{"/link", NoValue, CatPassthrough},
}

var msvcFlagsSorted []FlagSpec

func init() {
	msvcFlagsSorted = append(msvcFlagsSorted, msvcFlags...)
	sort.SliceStable(msvcFlagsSorted, func(i, j int) bool {
		return len(msvcFlagsSorted[i].Name) > len(msvcFlagsSorted[j].Name)
	})
}
