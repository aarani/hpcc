package scheduler

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Listen   string    `toml:"listen"`
	TLS      TLSConfig `toml:"tls"`
	Auth     Auth      `toml:"auth"`
	Routing  Routing   `toml:"routing"`
	Paranoid bool      `toml:"paranoid"`
}

type TLSConfig struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

type Auth struct {
	WorkerToken string   `toml:"worker_token"` // static token workers use to authenticate
	JWKS        JWKSAuth `toml:"jwks"`
}

type JWKSAuth struct {
	URL      string `toml:"url"`      // JWKS endpoint (e.g. https://idp.corp/.well-known/jwks.json)
	Issuer   string `toml:"issuer"`   // expected "iss" claim
	Audience string `toml:"audience"` // expected "aud" claim
}

type Routing struct {
	StickyTenants bool `toml:"sticky_tenants"`
}

func DefaultConfig() Config {
	return Config{
		Listen: ":9091",
		Auth: Auth{},
		Routing: Routing{
			StickyTenants: true,
		},
	}
}

// DefaultConfigPath returns ~/.config/hpcc/config.toml on Unix and the
// platform equivalent elsewhere.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "hpcc", "scheduler.toml"), nil
}

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

func (c Config) Validate() error {
	if c.TLS.CertFile == "" {
		return fmt.Errorf("tls.cert_file is required")
	}
	if c.TLS.KeyFile == "" {
		return fmt.Errorf("tls.key_file is required")
	}
	if c.Auth.WorkerToken == "" {
		return fmt.Errorf("auth.worker_token is required")
	}
	if len(c.Auth.WorkerToken) < 16 {
		return fmt.Errorf("auth.worker_token must be at least 16 characters")
	}
	if c.Auth.JWKS.URL == "" {
		return fmt.Errorf("auth.jwks.url is required")
	}
	if c.Auth.JWKS.Issuer == "" {
		return fmt.Errorf("auth.jwks.issuer is required")
	}
	if c.Auth.JWKS.Audience == "" {
		return fmt.Errorf("auth.jwks.audience is required")
	}
	return nil
}

