package runner

import (
	"os"

	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/config"
)

// LoadStores loads the config and returns the configured cache stores.
// Unlike NewContext, this does not require a compiler name — it is used
// by cache-management commands (stats, clean) that operate on stores
// directly.
func LoadStores() ([]store.Store, error) {
	path := os.Getenv("HPCC_CONFIG")
	if path == "" {
		p, err := config.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return store.FromConfig(cfg.Caches)
}
