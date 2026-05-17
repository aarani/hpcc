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
	Listen        string    `toml:"listen"`
	MetricsListen string    `toml:"metrics_listen"` // optional: HTTP /metrics scrape addr (e.g. ":9191"). Empty disables.
	TLS           TLSConfig `toml:"tls"`
	Auth          Auth      `toml:"auth"`
	Tenants       []Tenant  `toml:"tenant"`
	Routing       Routing   `toml:"routing"`
	Paranoid      bool      `toml:"paranoid"`
}

type TLSConfig struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

type Auth struct {
	WorkerToken string `toml:"worker_token"` // static token workers use to authenticate
}

// Tenant is one namespace boundary. A JWT carrying tenant_id = ID is
// validated against this entry's IdP (JWKSURL, Issuer, Audience). See
// docs/plan/multi-tenant.md for the threat model the per-tenant IdP closes.
type Tenant struct {
	ID       string `toml:"id"`
	Issuer   string `toml:"issuer"`
	JWKSURL  string `toml:"jwks_url"`
	TokenURL string `toml:"token_url"` // returned by GetTenantIdP so clients don't hardcode it
	Audience string `toml:"audience"`
	// ClientID and Scope are served back via GetTenantIdP so clients
	// don't carry them either. Both optional — empty fields are
	// omitted from the OAuth password-grant POST.
	ClientID string `toml:"client_id"`
	Scope    string `toml:"scope"`
}

type Routing struct {
	StickyTenants bool `toml:"sticky_tenants"`
}

func DefaultConfig() Config {
	return Config{
		Listen: ":9091",
		Auth:   Auth{},
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
	if len(c.Tenants) == 0 {
		return fmt.Errorf("at least one [[tenant]] entry is required (see docs/plan/multi-tenant.md)")
	}
	seen := make(map[string]struct{}, len(c.Tenants))
	for i, t := range c.Tenants {
		if t.ID == "" {
			return fmt.Errorf("tenant[%d].id is required", i)
		}
		if _, dup := seen[t.ID]; dup {
			return fmt.Errorf("tenant[%d].id %q is duplicated", i, t.ID)
		}
		seen[t.ID] = struct{}{}
		if t.Issuer == "" {
			return fmt.Errorf("tenant[%q].issuer is required", t.ID)
		}
		if t.JWKSURL == "" {
			return fmt.Errorf("tenant[%q].jwks_url is required", t.ID)
		}
		if t.TokenURL == "" {
			return fmt.Errorf("tenant[%q].token_url is required", t.ID)
		}
		if t.Audience == "" {
			return fmt.Errorf("tenant[%q].audience is required", t.ID)
		}
	}
	return nil
}
