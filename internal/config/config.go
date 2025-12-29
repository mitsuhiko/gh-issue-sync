package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Repository RepoConfig  `toml:"repository"`
	Local      LocalConfig `toml:"local"`
	Sync       SyncConfig  `toml:"sync"`
}

type RepoConfig struct {
	Owner string `toml:"owner"`
	Repo  string `toml:"repo"`
}

type LocalConfig struct {
	NextLocalID int `toml:"next_local_id"`
}

type SyncConfig struct {
	LastFullPull *time.Time `toml:"last_full_pull"`
}

func Default(owner, repo string) Config {
	return Config{
		Repository: RepoConfig{Owner: owner, Repo: repo},
		Local:      LocalConfig{NextLocalID: 1},
	}
}

func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err == nil {
		return cfg, nil
	} else if !strings.Contains(err.Error(), "last_full_pull") && !strings.Contains(err.Error(), "LastFullPull") {
		return cfg, err
	}

	legacy, err := loadLegacyConfig(data)
	if err != nil {
		return cfg, err
	}
	return legacy, nil
}

func Save(path string, cfg Config) error {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

type legacySyncConfig struct {
	LastFullPull *string `toml:"last_full_pull"`
}

type legacyConfig struct {
	Repository RepoConfig       `toml:"repository"`
	Local      LocalConfig      `toml:"local"`
	Sync       legacySyncConfig `toml:"sync"`
}

func loadLegacyConfig(data []byte) (Config, error) {
	var legacy legacyConfig
	if err := toml.Unmarshal(data, &legacy); err != nil {
		return Config{}, err
	}
	cfg := Config{
		Repository: legacy.Repository,
		Local:      legacy.Local,
	}
	if legacy.Sync.LastFullPull == nil {
		return cfg, nil
	}
	raw := strings.TrimSpace(*legacy.Sync.LastFullPull)
	if raw == "" {
		return cfg, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid last_full_pull %q: %w", raw, err)
	}
	cfg.Sync.LastFullPull = &parsed
	return cfg, nil
}
