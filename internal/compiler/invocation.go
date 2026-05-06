package compiler

import "github.com/aarani/hpcc/internal/enum"

// Invocation is the parsed form of one compile command. It is pure data,
// produced by a parser and consumed by a Compiler implementation.
type Invocation struct {
	Mode enum.InvocationMode

	Inputs []string // positional input files (.c/.cpp/.o/.a/...)
	Output string   // -o value, if any

	Includes       []string          // -I
	SystemIncludes []string          // -isystem, -iquote
	Defines        map[string]string // -D NAME[=VALUE]
	Undefines      []string          // -U
	Libraries      []string          // -l
	LibraryDirs    []string          // -L
	Std            string            // -std=...
	Optim          string            // -O...
	Debug          string            // -g... ("" if absent, "0"/"2"/etc otherwise)
	Warnings       []string          // -W... (e.g. "all", "no-foo")
	Features       []string          // -f... (e.g. "PIC", "no-rtti")
	Machine        []string          // -m... (e.g. "avx2", "arch=x86-64")
	Language       string            // -x value (forced language)

	// Passthrough holds flags we recognize but do nothing structured with —
	// linker/assembler/preprocessor passthrough, dep-info flags, -include, etc.
	Passthrough []string

	// Unknown holds flags that started with "-" but didn't match any spec.
	// They must be re-emitted verbatim when invoking the real compiler.
	Unknown []string

	// RawArgs is the argv we were given, after @file expansion.
	RawArgs []string
}

// NewInvocation returns an Invocation with maps initialized.
func NewInvocation() *Invocation {
	return &Invocation{Defines: map[string]string{}}
}

type InvocationResult struct {
	Stdout []byte
	Stderr []byte
	Err    error
}
