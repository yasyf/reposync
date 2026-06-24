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
	"github.com/yasyf/synckit/cregistry"
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

// RepoMeta is the per-repo payload carried by the convergent registry: where the
// repo lives (Relpath under the default location), its Trunk branch, and whether it
// is local-only. The registry key carries the repo's identity — its origin for a
// propagating repo, its relpath for a local-only one — so it is absent from this
// payload.
type RepoMeta struct {
	Relpath   string `json:"relpath"`
	Trunk     string `json:"trunk"`
	LocalOnly bool   `json:"local_only"`
}

// Repo is a tracked repository placed at Relpath under the host's default location.
// It is the in-memory view the sync, reconcile, and watch loops consume, rebuilt
// from a registry [RepoMeta] plus its key; Origin is empty for a local-only repo.
type Repo struct {
	Relpath   string
	Origin    string
	Trunk     string
	LocalOnly bool
}

// State is the reposync-owned slice of the shared on-disk configuration for this
// host. The self/hosts identity keys live alongside these in the same file but are
// owned by hostregistry; this struct neither carries nor persists them.
//
// Repos is the convergent registry of propagating (origin-bearing) repos, keyed by
// origin, that pull-merges across hosts; LocalRepos is the registry of local-only
// repos, keyed by relpath, that stays on this host and is never sent to peers. Both
// are LWW-Element-Set CRDTs, so add and remove converge and a removal propagates as
// a tombstone.
type State struct {
	DefaultLocation string
	Repos           cregistry.Registry[RepoMeta]
	LocalRepos      cregistry.Registry[RepoMeta]
	Settings        Settings
}

// New returns a State with empty registries and defaults applied, ready for AddRepo.
func New() *State {
	s := &State{}
	s.applyDefaults()
	return s
}

// AbsPath joins the repo's relpath onto an already-expanded default location.
func (r Repo) AbsPath(defaultLocationExpanded string) string {
	return filepath.Join(defaultLocationExpanded, r.Relpath)
}

// Now is the clock the registry stamps adds and removes by, indirected so tests can
// pin time; production stamps wall-clock microseconds.
var Now = time.Now

func repo(origin string, e cregistry.Entry[RepoMeta]) Repo {
	return Repo{Relpath: e.Value.Relpath, Origin: origin, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly}
}

// AllRepos returns every tracked repo present on this host — propagating repos keyed
// by origin plus local-only repos — as the flat [Repo] view the sync, reconcile, and
// watch loops iterate. Tombstoned (removed) entries are excluded.
func (s *State) AllRepos() []Repo {
	repos := make([]Repo, 0, len(s.Repos)+len(s.LocalRepos))
	for origin, e := range s.Repos.Present() {
		repos = append(repos, repo(origin, e))
	}
	for _, e := range s.LocalRepos.Present() {
		repos = append(repos, repo("", e))
	}
	return repos
}

// PropagatingRepos returns the present origin-bearing repos as the flat [Repo] view,
// excluding local-only repos and tombstones. These are the repos that converge across
// hosts.
func (s *State) PropagatingRepos() []Repo {
	repos := make([]Repo, 0, len(s.Repos))
	for origin, e := range s.Repos.Present() {
		repos = append(repos, repo(origin, e))
	}
	return repos
}

// AddRepo records r in the registry, stamping the add at the current time. A repo
// with an origin joins the propagating registry keyed by origin; a local-only repo
// joins the local registry keyed by relpath. A re-add after a tombstone carries a
// strictly-later stamp, so the repo becomes present again.
func (s *State) AddRepo(r Repo) {
	meta := RepoMeta{Relpath: r.Relpath, Trunk: r.Trunk, LocalOnly: r.LocalOnly}
	at := cregistry.UnixMicros(Now())
	if r.Origin == "" {
		s.LocalRepos.Add(r.Relpath, meta, at)
		return
	}
	s.Repos.Add(r.Origin, meta, at)
}

// RemoveRepo tombstones the repo registered at relpath, stamping the removal at the
// current time and keeping the entry, so the removal propagates via pull-merge and
// peers converge to absent. It searches the propagating registry first (by matching
// relpath), then the local registry (keyed by relpath).
func (s *State) RemoveRepo(relpath string) {
	at := cregistry.UnixMicros(Now())
	for origin, e := range s.Repos {
		if e.Value.Relpath == relpath {
			s.Repos.Remove(origin, at)
			return
		}
	}
	s.LocalRepos.Remove(relpath, at)
}

// FindRepoByOrigin returns the present repo with the given origin.
func (s *State) FindRepoByOrigin(origin string) (Repo, bool) {
	if e, ok := s.Repos[origin]; ok && e.Present() {
		return repo(origin, e), true
	}
	return Repo{}, false
}

// EncodeRepoRegistry marshals the propagating repo registry — origin-keyed entries
// including tombstones — to JSON. This is the cross-host wire form a peer reads to
// pull-merge; local-only repos are deliberately excluded, never leaving this host.
func (s *State) EncodeRepoRegistry() ([]byte, error) {
	return json.Marshal(s.Repos)
}

// DecodeRepoRegistry parses a peer's propagating repo registry from the JSON emitted
// by EncodeRepoRegistry, for the pull-merge fetch.
func DecodeRepoRegistry(data []byte) (cregistry.Registry[RepoMeta], error) {
	var reg cregistry.Registry[RepoMeta]
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("decode repo registry: %w", err)
	}
	return reg, nil
}

// Save persists this host's reposync-owned keys under the shared reconcile lock,
// foreign-key-preserving the self/hosts identity that hostregistry owns. It runs
// the one write codepath (Config.UpdateRaw) the package exposes.
func (s *State) Save() error {
	return Config.UpdateRaw(context.Background(), s.writeOwnedKeys)
}

// SaveReposUnlocked persists the propagating and local repo registries WITHOUT
// acquiring the reconcile lock, foreign-key-preserving every other key in the file
// (self, hosts, settings, default_location). It is for the convergent-reconcile
// pass, which already holds the lock around the whole pass and must not re-enter the
// non-reentrant flock; ordinary callers use Update/Save. The flock is the caller's
// responsibility.
func (s *State) SaveReposUnlocked() error {
	return Config.UpdateRawUnlocked(func(raw map[string]json.RawMessage) error {
		repos, err := json.Marshal(s.Repos)
		if err != nil {
			return fmt.Errorf("encode repos: %w", err)
		}
		localRepos, err := json.Marshal(s.LocalRepos)
		if err != nil {
			return fmt.Errorf("encode local_repos: %w", err)
		}
		raw["repos"] = repos
		raw["local_repos"] = localRepos
		return nil
	})
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
	localRepos, err := json.Marshal(s.LocalRepos)
	if err != nil {
		return fmt.Errorf("encode local_repos: %w", err)
	}
	settings, err := json.Marshal(s.Settings)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	raw["default_location"] = location
	raw["repos"] = repos
	raw["local_repos"] = localRepos
	raw["settings"] = settings
	return nil
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
	if s.Repos == nil {
		s.Repos = cregistry.New[RepoMeta]()
	}
	if s.LocalRepos == nil {
		s.LocalRepos = cregistry.New[RepoMeta]()
	}
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

// Load reads reposync's owned keys from the state file, returning defaults when it
// does not yet exist. It decodes through the same stateFromRaw path as Update, so
// the self/hosts identity keys that share the file are ignored here.
func Load() (*State, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is reposync's own state file under the fixed config dir, not user-supplied.
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return stateFromRaw(raw)
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
	if v, ok := raw["local_repos"]; ok {
		if err := json.Unmarshal(v, &s.LocalRepos); err != nil {
			return nil, fmt.Errorf("parse local_repos: %w", err)
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
