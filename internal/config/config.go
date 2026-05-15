package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/aarani/hpcc/internal/enum"
)

// Config is the on-disk configuration shape. TOML keys are snake_case;
// Go fields use the standard tag.
type Config struct {
	// SourceMode picks both the local cache-key derivation and the
	// dispatch wire format. The two have to track the same value so
	// client and worker compute matching keys for shared caches —
	// see enum.SourceMode for the full story.
	SourceMode enum.SourceMode `toml:"source_mode"`
	Caches     []CacheConfig   `toml:"cache"`
	Remote     RemoteConfig    `toml:"remote"`
}

// RemoteConfig drives the daemon's distributed-compile path. When
// Enabled is false (the default) the daemon stays local. When true,
// each compile request is routed through the scheduler to a worker;
// failures fall back to local execution with a warning.
//
// SourceMode lives on the parent Config, not here — it drives the
// local cache key too, not just dispatch, and a daemon with no
// [remote] block still needs a value for its cache key.
type RemoteConfig struct {
	Enabled     bool            `toml:"enabled"`
	TenantID    string          `toml:"tenant_id"`
	ImageRef    string          `toml:"image_ref"`
	ImageDigest string          `toml:"image_digest"`
	Scheduler   SchedulerConfig `toml:"scheduler"`
	OAuth       OAuthConfig     `toml:"oauth"`
}

// SchedulerConfig is the dial info for the scheduler gRPC endpoint.
// CAFile is optional — if empty, the system trust store is used.
type SchedulerConfig struct {
	URL    string `toml:"url"`
	CAFile string `toml:"ca_file"`
}

// OAuthConfig holds the bits needed to do an OAuth2 password grant
// against the IdP that fronts the scheduler. Password grant is chosen
// for headless usability — no browser redirect required.
type OAuthConfig struct {
	TokenURL     string `toml:"token_url"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	Username     string `toml:"username"`
	Password     string `toml:"password"`
	Scope        string `toml:"scope"`
}

// CacheConfig describes a single cache backend. The Type field selects
// which backend is used; the remaining fields are type-specific (only the
// fields relevant to the chosen type need to be set).
//
// TOML example (multiple caches):
//
//	[[cache]]
//	type     = "disk"
//	location = "/tmp/hpcc"
//	max_size = "10G"
//
//	[[cache]]
//	type     = "disk"
//	location = "/mnt/fast/hpcc"
//	max_size = "50G"
type CacheConfig struct {
	Type enum.CacheType `toml:"type"`

	// Disk-specific fields.
	Location string `toml:"location,omitempty"`
	MaxSize  string `toml:"max_size,omitempty"`

	// S3-specific fields.
	Bucket    string `toml:"bucket,omitempty"`
	Region    string `toml:"region,omitempty"`
	Endpoint  string `toml:"endpoint,omitempty"`
	AccessKey string `toml:"access_key,omitempty"`
	SecretKey string `toml:"secret_key,omitempty"`

	// AutoCreate, when true, has the worker attempt CreateBucket if
	// the bucket isn't reachable at startup. Only sane for local
	// MinIO/dev setups — in production the bucket is provisioned by
	// infra and the worker shouldn't even hold CreateBucket IAM
	// permissions. Default false.
	AutoCreate bool `toml:"auto_create,omitempty"`
}

// DefaultConfig returns the values used when no config file is present.
func DefaultConfig() Config {
	// Default: no caches configured. Require explicit TOML `[[cache]]`
	// entries to enable disk or S3 backends.
	// Default source mode is CAS: cache key uses the (cheap) dep
	// walk and dispatch (when [remote] is enabled) ships via CAS.
	return Config{SourceMode: enum.SourceModeCAS}
}

// DefaultConfigPath returns ~/.config/hpcc/config.toml on Unix and the
// platform equivalent elsewhere.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "hpcc", "config.toml"), nil
}

// LoadConfig reads a TOML config from path. A missing file is not an error
// — it returns DefaultConfig. A malformed file IS an error; the user
// should know their config is being ignored.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("stat config %q: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}
