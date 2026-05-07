package enum

import "fmt"

type CacheType int

const (
	CacheUnknown CacheType = iota
	CacheDisk
)

func (c CacheType) MarshalText() ([]byte, error) {
	switch c {
	case CacheDisk:
		return []byte("disk"), nil
	case CacheUnknown:
		return []byte(""), nil
	}
	return nil, fmt.Errorf("invalid cache type %d", c)
}

func (c *CacheType) UnmarshalText(text []byte) error {
	switch string(text) {
	case "disk":
		*c = CacheDisk
	case "":
		*c = CacheUnknown
	default:
		return fmt.Errorf("unknown cache type %q (want disk)", text)
	}
	return nil
}
