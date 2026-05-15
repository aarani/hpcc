package config

import "testing"

func TestParseSize_Suffixes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		// Plain bytes.
		{"", 0},
		{"0", 0},
		{"1", 1},
		{"4096", 4096},
		{"100B", 100},
		{"100b", 100},

		// Single-letter binary suffixes (legacy).
		{"1K", 1024},
		{"1k", 1024},
		{"2M", 2 << 20},
		{"3G", 3 << 30},
		{"1T", 1 << 40},

		// Colloquial two-letter suffixes (new).
		{"1KB", 1024},
		{"1kb", 1024},
		{"2MB", 2 << 20},
		{"4GB", 4 << 30},
		{"2GB", 2 << 30}, // the value DefaultConfig was hitting
		{"1TB", 1 << 40},

		// Explicit IEC binary suffixes.
		{"1KiB", 1024},
		{"1kib", 1024},
		{"2MiB", 2 << 20},
		{"5GiB", 5 << 30},

		// Whitespace between number and unit.
		{"4 GB", 4 << 30},
		{"  10  G  ", 10 << 30},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseSize_RejectsBadInput(t *testing.T) {
	cases := []string{
		"abc",   // no digits at all
		"1XB",   // unknown unit
		"1.5G",  // no fractional support
		"-1G",   // leading sign not in digit set
		"1 GBs", // unknown unit "gbs"
		"GB",    // suffix only
		"1Z",    // unknown unit
	}
	for _, in := range cases {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want error", in)
		}
	}
}

func TestParseSize_OverflowGuarded(t *testing.T) {
	// 9_000_000_000 TiB would overflow int64 (~8 EiB max).
	if _, err := ParseSize("9000000000T"); err == nil {
		t.Error("expected overflow error for 9000000000T")
	}
}
