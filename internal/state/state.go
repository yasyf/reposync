// Package state loads and persists the reposync-owned slice of the shared JSON
// state file: the repos, settings, and default_location keys the registration
// commands mutate and the sync, watch, and reconcile commands read.
//
// The host-identity slice of that file (self, hosts) is owned by the public
// github.com/yasyf/synckit/hostregistry package, which reposync drives through
// state.Config; this package never reads or writes those keys. Every write here
// goes through hostregistry's foreign-key-preserving Config.UpdateRaw, so a
// reposync write leaves self/hosts byte-for-byte intact and a hostregistry write
// leaves repos/settings/default_location intact — both share one flock and one
// on-disk schema. The path/lock primitives also live in hostregistry; this
// package forwards to them.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/synckit/codec"
	"github.com/yasyf/synckit/hostregistry"
)

// ToolName is reposync's CLI/config identity: the single source for the
// hostregistry Config that selects ~/.config/reposync and the verify/install
// probes. Every reposync wrapper over hostregistry reuses Config rather than
// re-spelling the name.
const ToolName = "reposync"

const (
	defaultLocation      = "~/Code"
	defaultInterval      = 15 * time.Minute
	defaultIdleThreshold = 5 * time.Minute
	defaultWatchDebounce = 3 * time.Second
	defaultRepoOpTimeout = 2 * time.Minute
	defaultPushAfter     = 24 * time.Hour
)

// Config is reposync's host-registry handle, naming the tool so hostregistry
// resolves the config dir and the ssh probes. State and host both drive it.
var Config = hostregistry.Config{Name: ToolName}

// ErrLockBusy is returned when the reconcile lock is held past the caller's deadline.
var ErrLockBusy = hostregistry.ErrLockBusy

// Duration is the canonical Go-duration string codec, re-exported from
// github.com/yasyf/synckit/codec so callers in this module spell it state.Duration.
type Duration = codec.Duration

// Settings holds the cadence knobs read by the sync, reconcile, and watch loops.
type Settings struct {
	Interval      codec.Duration `json:"interval"`
	IdleThreshold codec.Duration `json:"idle_threshold"`
	WatchDebounce codec.Duration `json:"watch_debounce"`
	RepoOpTimeout codec.Duration `json:"repo_op_timeout"`
	PushAfter     codec.Duration `json:"push_after"`
}

// Repo is a tracked repository placed at Relpath under the host's default location.
type Repo struct {
	Relpath   string `json:"relpath"`
	Origin    string `json:"origin"`
	Trunk     string `json:"trunk"`
	LocalOnly bool   `json:"local_only"`
}

// State is the reposync-owned slice of the shared on-disk configuration for this
// host. The self/hosts identity keys live alongside these in the same file but
// are owned by hostregistry; this struct neither carries nor persists them.
type State struct {
	DefaultLocation string   `json:"default_location"`
	Repos           []Repo   `json:"repos"`
	Settings        Settings `json:"settings"`
}

// AbsPath joins the repo's relpath onto an already-expanded default location.
func (r Repo) AbsPath(defaultLocationExpanded string) string {
	return filepath.Join(defaultLocationExpanded, r.Relpath)
}

// Save persists this host's reposync-owned keys under the shared reconcile lock,
// foreign-key-preserving the self/hosts identity that hostregistry owns. It runs
// the one write codepath (Config.UpdateRaw) the package exposes.
func (s *State) Save() error {
	return Config.UpdateRaw(context.Background(), s.writeOwnedKeys)
}

// writeOwnedKeys marshals the reposync-owned keys into the shared raw state map,
// leaving every other key (self, hosts) untouched.
func (s *State) writeOwnedKeys(raw map[string]json.RawMessage) error {
	location, err := json.Marshal(s.DefaultLocation)
	if err != nil {
		return fmt.Errorf("encode default_location: %w", err)
	}
	repos, err := json.Marshal(s.Repos)
	if err != nil {
		return fmt.Errorf("encode repos: %w", err)
	}
	settings, err := json.Marshal(s.Settings)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	raw["default_location"] = location
	raw["repos"] = repos
	raw["settings"] = settings
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
	if s.Settings.RepoOpTimeout == 0 {
		s.Settings.RepoOpTimeout = Duration(defaultRepoOpTimeout)
	}
	if s.Settings.PushAfter == 0 {
		s.Settings.PushAfter = Duration(defaultPushAfter)
	}
}

// Dir returns the reposync config directory under XDG_CONFIG_HOME or ~/.config.
func Dir() (string, error) {
	return Config.Dir()
}

// Path returns the absolute path to the state.json file.
func Path() (string, error) {
	return Config.Path()
}

// SockPath returns the absolute path to the daemon's RPC unix socket.
func SockPath() (string, error) {
	return Config.SockPath()
}

// Load reads reposync's owned keys from the state file, returning defaults when
// it does not yet exist. The self/hosts identity keys share the file but are
// owned by hostregistry, so they are ignored here.
func Load() (*State, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is reposync's own state file under the fixed config dir, not user-supplied.
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
// then persists only reposync's owned keys, leaving the self/hosts identity that
// hostregistry owns byte-for-byte intact. It serializes read-modify-write across
// processes through the one canonical flock that hostregistry writers also hold.
func Update(ctx context.Context, fn func(*State) error) (*State, error) {
	var out *State
	err := Config.UpdateRaw(ctx, func(raw map[string]json.RawMessage) error {
		st, err := stateFromRaw(raw)
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		if err := st.writeOwnedKeys(raw); err != nil {
			return err
		}
		out = st
		return nil
	})
	return out, err
}

// stateFromRaw decodes reposync's owned keys out of the shared raw state map and
// applies defaults, leaving self/hosts to hostregistry.
func stateFromRaw(raw map[string]json.RawMessage) (*State, error) {
	s := &State{}
	if v, ok := raw["default_location"]; ok {
		if err := json.Unmarshal(v, &s.DefaultLocation); err != nil {
			return nil, fmt.Errorf("parse default_location: %w", err)
		}
	}
	if v, ok := raw["repos"]; ok {
		if err := json.Unmarshal(v, &s.Repos); err != nil {
			return nil, fmt.Errorf("parse repos: %w", err)
		}
	}
	if v, ok := raw["settings"]; ok {
		if err := json.Unmarshal(v, &s.Settings); err != nil {
			return nil, fmt.Errorf("parse settings: %w", err)
		}
	}
	s.applyDefaults()
	return s, nil
}

// WithLock runs fn while holding an exclusive flock on the reconcile lock file,
// giving up with ErrLockBusy once ctx is done so a contended acquire fails fast
// instead of blocking on a wedged holder.
func WithLock(ctx context.Context, fn func() error) error {
	return Config.WithLock(ctx, fn)
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
