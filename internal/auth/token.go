// Package auth stores the credentials hpcc uses for distributed
// compilation. The on-disk token file is written by `hpcc auth login`
// and read by the daemon's dispatcher — keeping passwords out of
// config.toml and off the user's filesystem long-term.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Token is the cached result of a successful OAuth2 password grant.
// We persist the refresh_token (when the IdP returns one) so the
// daemon can renew expired access tokens silently; if it can't, the
// dispatcher surfaces a "run `hpcc auth login`" error.
//
// ClientSecret is held alongside the rest because the daemon needs
// it to drive the refresh grant and the only place a user would
// have supplied it is `hpcc auth login --client-secret=…`. Most
// public-grant IdPs leave it empty.
type Token struct {
	Username     string    `json:"username"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	ClientSecret string    `json:"client_secret,omitempty"`
}

// ExpiredWithin reports whether the access token has less than skew
// of life remaining. Callers refresh proactively rather than waiting
// for the IdP to reject — an in-flight compile that fails on a
// just-expired token is a poor UX.
func (t Token) ExpiredWithin(skew time.Duration) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(skew).After(t.ExpiresAt)
}

// DefaultPath returns the canonical token path, sibling of
// config.toml. Honors XDG_CONFIG_HOME on Unix.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "hpcc", "token.json"), nil
}

// Load reads the token file. Returns fs.ErrNotExist (wrapped) when
// no token has been saved yet; callers should treat that as
// "the user has not run `hpcc auth login` on this machine".
func Load(path string) (Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, fmt.Errorf("parse token %q: %w", path, err)
	}
	return t, nil
}

// Save writes the token atomically with 0600 perms. The rename
// guards against a crash mid-write leaving a truncated token —
// "no token" is recoverable (re-login), "garbage token" is not.
func Save(path string, t Token) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir for token: %w", err)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename token into place: %w", err)
	}
	return nil
}

// Delete removes the token file. A missing file is not an error
// — `hpcc auth logout` is idempotent.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
