// Package state loads and persists the reposync JSON state file that the
// registration commands mutate and the sync, watch, and reconcile commands read.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	configSubdir = "reposync"
	stateFile    = "state.json"
	lockFile     = "reconcile.lock"

	defaultLocation      = "~/Code"
	defaultInterval      = 15 * time.Minute
	defaultIdleThreshold = 5 * time.Minute
	defaultWatchDebounce = 3 * time.Second
)

// Duration is a time.Duration that marshals to and from a JSON string such as "15m".
type Duration time.Duration

// Settings holds the cadence knobs read by the sync, reconcile, and watch loops.
type Settings struct {
	Interval      Duration `json:"interval"`
	IdleThreshold Duration `json:"idle_threshold"`
	WatchDebounce Duration `json:"watch_debounce"`
}

// Repo is a tracked repository placed at Relpath under the host's default location.
type Repo struct {
	Relpath   string `json:"relpath"`
	Origin    string `json:"origin"`
	Trunk     string `json:"trunk"`
	LocalOnly bool   `json:"local_only"`
}

// State is the full on-disk reposync configuration for this host.
type State struct {
	DefaultLocation string   `json:"default_location"`
	Self            string   `json:"self"`
	Hosts           []string `json:"hosts"`
	Repos           []Repo   `json:"repos"`
	Settings        Settings `json:"settings"`
}

// MarshalJSON encodes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a Go duration string, rejecting anything unparseable.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("decode duration: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// AbsPath joins the repo's relpath onto an already-expanded default location.
func (r Repo) AbsPath(defaultLocationExpanded string) string {
	return filepath.Join(defaultLocationExpanded, r.Relpath)
}

// Save writes the state atomically: a temp file in the state dir renamed over the target.
func (s *State) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename state into place: %w", err)
	}
	return nil
}

// UpsertRepo adds or replaces a repo, keyed on origin, or on relpath when origin is empty.
func (s *State) UpsertRepo(r Repo) {
	for i := range s.Repos {
		if repoMatches(s.Repos[i], r) {
			s.Repos[i] = r
			return
		}
	}
	s.Repos = append(s.Repos, r)
}

// RemoveRepo drops the repo registered at relpath.
func (s *State) RemoveRepo(relpath string) {
	kept := make([]Repo, 0, len(s.Repos))
	for _, r := range s.Repos {
		if r.Relpath != relpath {
			kept = append(kept, r)
		}
	}
	s.Repos = kept
}

// FindRepoByOrigin returns the registered repo with the given origin.
func (s *State) FindRepoByOrigin(origin string) (*Repo, bool) {
	for i := range s.Repos {
		if s.Repos[i].Origin == origin {
			return &s.Repos[i], true
		}
	}
	return nil, false
}

// UpsertHost adds a peer ssh target unless it is already registered.
func (s *State) UpsertHost(target string) {
	for _, h := range s.Hosts {
		if h == target {
			return
		}
	}
	s.Hosts = append(s.Hosts, target)
}

// RemoveHost drops a peer ssh target.
func (s *State) RemoveHost(target string) {
	kept := make([]string, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		if h != target {
			kept = append(kept, h)
		}
	}
	s.Hosts = kept
}

// DefaultLocationExpanded resolves the default location to an absolute path with ~ expanded.
func (s *State) DefaultLocationExpanded() (string, error) {
	expanded, err := expandHome(s.DefaultLocation)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

func (s *State) applyDefaults() {
	if s.DefaultLocation == "" {
		s.DefaultLocation = defaultLocation
	}
	if s.Settings.Interval == 0 {
		s.Settings.Interval = Duration(defaultInterval)
	}
	if s.Settings.IdleThreshold == 0 {
		s.Settings.IdleThreshold = Duration(defaultIdleThreshold)
	}
	if s.Settings.WatchDebounce == 0 {
		s.Settings.WatchDebounce = Duration(defaultWatchDebounce)
	}
}

// Dir returns the reposync config directory under XDG_CONFIG_HOME or ~/.config.
func Dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, configSubdir), nil
}

// Path returns the absolute path to the state.json file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stateFile), nil
}

// Load reads the state file, returning defaults when it does not yet exist.
func Load() (*State, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s := &State{}
		s.applyDefaults()
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	s.applyDefaults()
	return &s, nil
}

// Update runs fn against a freshly loaded State under the reconcile-lock flock,
// then atomically saves it. Serializes read-modify-write across processes.
func Update(fn func(*State) error) (*State, error) {
	var out *State
	err := WithLock(func() error {
		st, err := Load()
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		if err := st.Save(); err != nil {
			return err
		}
		out = st
		return nil
	})
	return out, err
}

// WithLock runs fn while holding an exclusive flock on the reconcile lock file.
func WithLock(fn func() error) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	lock := flock.New(filepath.Join(dir, lockFile))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("acquire reconcile lock: %w", err)
	}
	defer lock.Unlock()
	return fn()
}

func repoMatches(existing, incoming Repo) bool {
	if incoming.Origin != "" {
		return existing.Origin == incoming.Origin
	}
	return existing.Relpath == incoming.Relpath
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
