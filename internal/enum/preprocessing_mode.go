package enum

import "fmt"

type PreprocessingMode int

const (
	PreprocessUnknown PreprocessingMode = iota
	PreprocessRemote
	PreprocessLocal
)

// MarshalText / UnmarshalText let this enum round-trip through TOML
// (and JSON, YAML) as a human-readable string instead of an int.
//
// "local"  → PreprocessLocal:  client preprocesses, ships preprocessed bytes.
// "remote" → PreprocessRemote: client ships dep closure, worker preprocesses.

func (m PreprocessingMode) MarshalText() ([]byte, error) {
	switch m {
	case PreprocessLocal:
		return []byte("local"), nil
	case PreprocessRemote:
		return []byte("remote"), nil
	case PreprocessUnknown:
		return []byte(""), nil
	}
	return nil, fmt.Errorf("invalid preprocessing mode %d", m)
}

func (m *PreprocessingMode) UnmarshalText(text []byte) error {
	switch string(text) {
	case "local":
		*m = PreprocessLocal
	case "remote":
		*m = PreprocessRemote
	case "":
		*m = PreprocessUnknown
	default:
		return fmt.Errorf("unknown preprocessing mode %q (want local|remote)", text)
	}
	return nil
}
