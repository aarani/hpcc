package config

import (
	"fmt"
	"strconv"
	"strings"
)

// sizeSuffixes maps every accepted suffix (lowercased, whitespace-trimmed)
// to its byte multiplier. K/M/G/T, the explicit binary IEC names
// (KiB/MiB/GiB/TiB), and the colloquial KB/MB/GB/TB are all treated as
// binary — strict-decimal "MB = 1,000,000" semantics are surprising in
// software-config contexts where 4MB historically means 4*1024*1024.
// "B" alone is bytes; the empty suffix is also bytes for plain numbers.
var sizeSuffixes = map[string]int64{
	"":    1,
	"b":   1,
	"k":   1 << 10,
	"kb":  1 << 10,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1 << 20,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1 << 30,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1 << 40,
	"tib": 1 << 40,
}

// ParseSize converts a human-readable size string (e.g. "10G", "500MB",
// "1.5GiB" — actually no, integer only — "1024K", "4096") into bytes.
// Suffixes are case-insensitive; whitespace between number and suffix
// is allowed. A plain number is bytes. An empty string returns 0,
// which the cache layer interprets as "unlimited."
//
// All multipliers are binary (1024-based), including KB/MB/GB/TB. If
// you need strict-decimal SI semantics you'll have to ask for them
// separately; nobody asks the size of their RAM stick in base 10.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Split into digit prefix and unit suffix. Walking byte-by-byte
	// keeps this allocation-free and avoids the regex tax for what is
	// really just "scan a number and look up a unit."
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("parse size %q: no leading digits", s)
	}

	numStr := s[:n]
	suffix := strings.ToLower(strings.TrimSpace(s[n:]))

	multiplier, ok := sizeSuffixes[suffix]
	if !ok {
		return 0, fmt.Errorf("parse size %q: unknown unit %q", s, s[n:])
	}

	val, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}

	// Overflow check before the multiply. int64 maxes out at ~9.2 EiB,
	// so a TiB-suffixed input over ~8 million would otherwise wrap.
	if multiplier > 1 && val > (1<<62)/multiplier {
		return 0, fmt.Errorf("parse size %q: result would overflow int64", s)
	}
	return val * multiplier, nil
}
