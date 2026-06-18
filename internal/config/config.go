// Package config loads and validates the reposync configuration file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultRemote   = "origin"
	defaultInterval = 5 * time.Minute
)

// Duration is a time.Duration that unmarshals from a YAML string like "5m".
type Duration time.Duration

// UnmarshalYAML parses a duration string into a Duration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// AsDuration returns the underlying time.Duration.
func (d Duration) AsDuration() time.Duration {
	return time.Duration(d)
}

// Repo is a single repository to keep in sync with one of its git remotes.
type Repo struct {
	// Path is the local working tree, with ~ expanded at load time.
	Path string `yaml:"path"`
	// Remote is the git remote to sync against (default "origin").
	Remote string `yaml:"remote"`
	// Branch is the branch to sync (default: the repo's current branch).
	Branch string `yaml:"branch"`
	// AutoCommit commits a dirty working tree before syncing when true.
	AutoCommit bool `yaml:"auto_commit"`
}

// Config is the full reposync configuration.
type Config struct {
	// Interval is how often the launchd timer triggers a sync.
	Interval Duration `yaml:"interval"`
	// Repos is the set of repositories to keep in sync.
	Repos []Repo `yaml:"repos"`
}

// DefaultPath returns the config path under the user config directory.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "reposync", "config.yaml")
}

// Load reads, validates, and normalizes the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Interval == 0 {
		cfg.Interval = Duration(defaultInterval)
	}
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("config %s lists no repos", path)
	}

	for i := range cfg.Repos {
		repo := &cfg.Repos[i]
		if repo.Path == "" {
			return nil, fmt.Errorf("repo %d: path is required", i)
		}
		expanded, err := expandHome(repo.Path)
		if err != nil {
			return nil, fmt.Errorf("repo %d: %w", i, err)
		}
		repo.Path = expanded
		if repo.Remote == "" {
			repo.Remote = defaultRemote
		}
	}

	return &cfg, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !hasHomePrefix(path) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~ in %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func hasHomePrefix(path string) bool {
	return len(path) >= 2 && path[0] == '~' && (path[1] == '/' || path[1] == filepath.Separator)
}
