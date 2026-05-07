package compiler

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/aarani/hpcc/internal/enum"
	"github.com/zeebo/blake3"
)

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

// GetBytes computes the cache-key seed for this invocation: a 32-byte
// BLAKE3-256 digest mixing source content, compiler identity, and the
// cache-key-relevant flags.
//
// Two preprocessing strategies, both producing the same shape of output:
//
//   - PreprocessRemote (manifest mode): hash the contents of all input
//     and dependency files. Used when the worker will preprocess on its
//     side and we just need to enumerate inputs locally.
//   - default (preprocess mode): run the preprocessor locally and hash
//     the resulting source bytes (via the digest already computed in
//     PreprocessResult — single pass over the bytes, not two).
//
// Each chunk written to the hasher is length-prefixed so concatenation
// can't collide ("ab"+"c" hashes differently from "a"+"bc").
func (inv *Invocation) CacheKey(ctx Context) ([]byte, error) {
	if len(inv.Inputs) == 0 {
		return nil, fmt.Errorf("no input files in invocation")
	}

	compilerIdentity, err := ctx.Compiler.Identity()
	if err != nil {
		return nil, fmt.Errorf("get compiler identity: %w", err)
	}

	digest := blake3.New()
	writeChunk := func(data []byte) {
		var lenbuf [8]byte
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(data)))
		digest.Write(lenbuf[:])
		digest.Write(data)
	}

	if ctx.Config.PreprocessingMode == enum.PreprocessRemote {
		deps, err := ctx.Compiler.FindDependencies(inv)
		if err != nil {
			return nil, err
		}
		inputs := slices.Clone(inv.Inputs)
		slices.Sort(inputs)
		sortedDeps := slices.Clone(deps)
		slices.Sort(sortedDeps)
		for _, path := range append(inputs, sortedDeps...) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read dependency %q: %w", path, err)
			}
			writeChunk(data)
		}
	} else {
		res, err := ctx.Compiler.Preprocess(inv)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("preprocessor exit %d: %s", res.ExitCode, res.Stderr)
		}
		// Source bytes are already digested into res.Digest; reuse it
		// instead of re-hashing the whole preprocessed source.
		writeChunk(res.Digest[:])
	}

	writeChunk(compilerIdentity)
	writeChunk(cacheKeyFlags(inv))

	return digest.Sum(nil), nil
}

// ComputeHash runs the preprocessor and returns a hex-encoded BLAKE3-256
// digest of the preprocessed source. The digest is computed during
// preprocessing (PreprocessResult.Digest) so this is a single pass over the
// bytes, not two.
func (inv *Invocation) ComputeHash(ctx Context) (string, error) {
	res, err := inv.CacheKey(ctx)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(res), nil
}

// InvocationResult is the captured output of a single compile run. It is
// what the cache stores on a miss and replays on a hit; the duration is
// recorded for metadata only and is not part of the cache key.
type InvocationResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
	Err      error
}
