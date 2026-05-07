package compiler

import (
	"bytes"
	"fmt"
	"slices"
)

// cacheKeyFlags returns a canonical byte encoding of the parts of an
// Invocation that affect the compiled object's content. Anything that
// only affects diagnostics, dependency-file emission, or where the
// output lands is excluded — different values shouldn't poison the cache.
//
// Encoding rules:
//   - Each section starts with a tag, then null-separated entries, then
//     a trailing null. Tags are fixed strings, never user-controlled, so
//     no escaping is needed.
//   - Defines is sorted by key (the order of -D doesn't matter
//     semantically; -DFOO -DBAR == -DBAR -DFOO).
//   - Other lists are kept in original order. Order CAN matter: include
//     search order resolves header ambiguity, and -fno-X then -fX is
//     different from the reverse. Better to over-key than under-key.
func cacheKeyFlags(inv *Invocation) []byte {
	var b bytes.Buffer

	writeList := func(tag string, items []string) {
		b.WriteString(tag)
		b.WriteByte(0)
		for _, item := range items {
			b.WriteString(item)
			b.WriteByte(0)
		}
		b.WriteByte(0)
	}
	writeKV := func(tag, value string) {
		b.WriteString(tag)
		b.WriteByte('=')
		b.WriteString(value)
		b.WriteByte(0)
	}

	writeList("I", inv.Includes)
	writeList("isystem", inv.SystemIncludes)
	writeList("U", inv.Undefines)

	// Defines: sorted-by-key for canonical order.
	defKeys := make([]string, 0, len(inv.Defines))
	for k := range inv.Defines {
		defKeys = append(defKeys, k)
	}
	slices.Sort(defKeys)
	b.WriteString("D\x00")
	for _, k := range defKeys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(inv.Defines[k])
		b.WriteByte(0)
	}
	b.WriteByte(0)

	writeKV("std", inv.Std)
	writeKV("opt", inv.Optim)
	writeKV("dbg", inv.Debug)
	writeKV("lang", inv.Language)
	writeKV("mode", fmt.Sprintf("%d", inv.Mode))

	writeList("W", inv.Warnings)
	writeList("f", inv.Features)
	writeList("m", inv.Machine)

	writeList("pass", filterIrrelevantPassthrough(inv.Passthrough))
	writeList("unk", inv.Unknown)

	// Intentionally excluded:
	//   - Output: only file content matters, not the path.
	//   - Inputs: hashed separately (preprocess bytes or dep contents).
	//   - Libraries / LibraryDirs: irrelevant in compile mode (they only
	//     affect link-time). If we ever cache link products, revisit.

	return b.Bytes()
}

// filterIrrelevantPassthrough drops flags from inv.Passthrough that don't
// affect the produced object — chiefly dep-info emission flags. Two
// invocations differing only in "-MF deps.d" vs "-MF other.d" should hit
// the same cache entry.
func filterIrrelevantPassthrough(passthrough []string) []string {
	out := make([]string, 0, len(passthrough))
	skipNext := false
	for _, a := range passthrough {
		if skipNext {
			skipNext = false
			continue
		}
		switch a {
		case "-MD", "-MMD":
			continue // dep file emission, no .o impact
		case "-MF", "-MT", "-MQ":
			skipNext = true // takes a separate value; skip both
			continue
		case "/showIncludes":
			continue // MSVC dep emission to stderr
		}
		out = append(out, a)
	}
	return out
}
