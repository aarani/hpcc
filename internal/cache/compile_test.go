package cache

import (
	"bytes"
	"reflect"
	"testing"
)

func TestEncodeExtras_roundTrip(t *testing.T) {
	in := map[string][]byte{
		"build/main.d":           []byte("main.o: main.c header.h\n"),
		"scripts/mod/.empty.o.d": []byte("empty.o: empty.c\n"),
		"binary.bin":             {0x00, 0x01, 0x02, 0xff, 0x7f, 0x80},
		"":                       []byte("zero-len key edge case"),
		"empty-value":            nil,
	}
	encoded := encodeExtras(in)
	out, err := decodeExtras(encoded)
	if err != nil {
		t.Fatalf("decodeExtras: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("entry count: got %d, want %d", len(out), len(in))
	}
	for k, vIn := range in {
		vOut, ok := out[k]
		if !ok {
			t.Errorf("key %q missing after round-trip", k)
			continue
		}
		// nil and empty byte slices both have len 0; treat as equal.
		if !bytes.Equal(vIn, vOut) {
			t.Errorf("value for %q: got %q, want %q", k, vOut, vIn)
		}
	}
}

func TestEncodeExtras_deterministicOrder(t *testing.T) {
	// Same map content must produce identical bytes regardless of
	// map iteration order. encodeExtras sorts by path to guarantee
	// this — important so two compiles that produced the same extras
	// don't churn the cache layer (e.g. different on-disk byte
	// patterns confusing dedupe / external mirroring).
	in := map[string][]byte{
		"zzz/late.d":  []byte("late"),
		"aaa/early.d": []byte("early"),
		"mmm/mid.d":   []byte("mid"),
	}
	const n = 20
	first := encodeExtras(in)
	for i := 0; i < n; i++ {
		got := encodeExtras(in)
		if !bytes.Equal(got, first) {
			t.Fatalf("encode is non-deterministic on iteration %d", i)
		}
	}
}

func TestDecodeExtras_rejectsTruncatedInput(t *testing.T) {
	in := map[string][]byte{"foo": []byte("bar")}
	full := encodeExtras(in)
	// Try every truncation. None should panic; all should error.
	for cut := 0; cut < len(full); cut++ {
		_, err := decodeExtras(full[:cut])
		if err == nil {
			t.Errorf("truncating to %d bytes should have errored", cut)
		}
	}
}

func TestDecodeExtras_emptyMap(t *testing.T) {
	out, err := decodeExtras(encodeExtras(map[string][]byte{}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %v", out)
	}
}

// Direct comparison vs JSON+base64 to document the size win. The
// pinned ratio isn't a load-bearing invariant; just a quick check
// that the binary format is actually smaller (it should be for any
// non-trivial payload).
func TestEncodeExtras_smallerThanJSON(t *testing.T) {
	in := map[string][]byte{
		"build/main.d": bytes.Repeat([]byte{0xff, 0x00}, 512),
	}
	binSize := len(encodeExtras(in))
	// base64 of 1024 bytes is ~1368 chars; JSON wrapping adds more.
	// We just assert binary is meaningfully smaller.
	if binSize >= 1100 {
		t.Errorf("binary encoding %d bytes is suspiciously large for 1KiB payload", binSize)
	}
}

// Reflect-based safety net: encode/decode preserves type shape.
func TestEncodeExtras_preservesMapType(t *testing.T) {
	in := map[string][]byte{"x": []byte("y")}
	out, err := decodeExtras(encodeExtras(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reflect.TypeOf(out) != reflect.TypeOf(in) {
		t.Errorf("type drift: got %T, want %T", out, in)
	}
}
