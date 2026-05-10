package store

import (
	"fmt"

	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
)

// FromConfig builds the configured cache stores from a slice of
// CacheConfig entries, in the order they appear. Both the client (via
// runner) and the worker (in paranoid mode) call this so the cache
// schema lives in exactly one place.
func FromConfig(cfgs []config.CacheConfig) ([]Store, error) {
	stores := make([]Store, 0, len(cfgs))
	for _, c := range cfgs {
		switch c.Type {
		case enum.CacheDisk:
			ds, err := NewDiskCacheStore(c.Location, c.MaxSize)
			if err != nil {
				return nil, fmt.Errorf("init disk cache at %q: %w", c.Location, err)
			}
			stores = append(stores, ds)
		case enum.CacheS3:
			ss, err := NewS3CacheStore(c.Bucket, c.MaxSize, c.Region, c.Endpoint, c.AccessKey, c.SecretKey)
			if err != nil {
				return nil, fmt.Errorf("init s3 cache for %q: %w", c.Bucket, err)
			}
			stores = append(stores, ss)
		default:
			return nil, fmt.Errorf("unsupported cache type %d", c.Type)
		}
	}
	return stores, nil
}
