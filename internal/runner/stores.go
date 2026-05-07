package runner

import (
	"fmt"
	"os"

	"github.com/aarani/hpcc/internal"
	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/enum"
)

// LoadStores loads the config and returns the configured cache stores.
// Unlike NewContext, this does not require a compiler name — it is used
// by cache-management commands (stats, clean) that operate on stores
// directly.
func LoadStores() ([]*store.DiskCacheStore, error) {
	path := os.Getenv("HPCC_CONFIG")
	if path == "" {
		p, err := internal.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := internal.LoadConfig(path)
	if err != nil {
		return nil, err
	}

	var stores []*store.DiskCacheStore
	for _, cacheCfg := range cfg.Caches {
		switch cacheCfg.Type {
		case enum.CacheDisk:
			ds, err := store.NewDiskCacheStore(cacheCfg.Location, cacheCfg.MaxSize)
			if err != nil {
				return nil, fmt.Errorf("init disk cache: %w", err)
			}
			stores = append(stores, ds)
		default:
			return nil, fmt.Errorf("unsupported cache type %q", cacheCfg.Type)
		}
	}
	return stores, nil
}
