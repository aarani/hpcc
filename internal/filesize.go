package internal

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize converts a human-readable size string (e.g. "10G", "500M",
// "1024K", "4096") into bytes. Supports suffixes K, M, G, T (case-
// insensitive, binary units). A plain number is treated as bytes.
// An empty string returns 0 (unlimited).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	multiplier := int64(1)
	suffix := s[len(s)-1]
	switch suffix {
	case 'k', 'K':
		multiplier = 1 << 10
		s = s[:len(s)-1]
	case 'm', 'M':
		multiplier = 1 << 20
		s = s[:len(s)-1]
	case 'g', 'G':
		multiplier = 1 << 30
		s = s[:len(s)-1]
	case 't', 'T':
		multiplier = 1 << 40
		s = s[:len(s)-1]
	}

	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("parse size: negative value %d", n)
	}
	return n * multiplier, nil
}
