package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConfig(t, "repos:\n  - path: /tmp/example\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval.AsDuration() != 5*time.Minute {
		t.Errorf("interval = %s, want 5m", cfg.Interval.AsDuration())
	}
	if got := cfg.Repos[0].Remote; got != "origin" {
		t.Errorf("remote = %q, want origin", got)
	}
}

func TestLoadParsesInterval(t *testing.T) {
	path := writeConfig(t, "interval: 90s\nrepos:\n  - path: /tmp/example\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval.AsDuration() != 90*time.Second {
		t.Errorf("interval = %s, want 90s", cfg.Interval.AsDuration())
	}
}

func TestLoadExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	path := writeConfig(t, "repos:\n  - path: ~/Code/example\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Repos[0].Path; got != "/home/tester/Code/example" {
		t.Errorf("path = %q, want /home/tester/Code/example", got)
	}
}

func TestLoadRejectsEmptyRepos(t *testing.T) {
	path := writeConfig(t, "interval: 5m\n")

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for config with no repos")
	}
}

func TestLoadRejectsRepoWithoutPath(t *testing.T) {
	path := writeConfig(t, "repos:\n  - remote: origin\n")

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for repo without path")
	}
}

func TestLoadRejectsBadInterval(t *testing.T) {
	path := writeConfig(t, "interval: soon\nrepos:\n  - path: /tmp/example\n")

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unparseable interval")
	}
}
