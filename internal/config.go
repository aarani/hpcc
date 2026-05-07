package internal

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
	PreprocessingMode enum.PreprocessingMode `toml:"preprocessing_mode"`
}

// DefaultConfig returns the values used when no config file is present.
func DefaultConfig() Config {
	return Config{
		PreprocessingMode: enum.PreprocessLocal,
	}
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
