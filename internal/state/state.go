// Package state owns repo-sync's exact schema v1 product payload inside the
// shared hostregistry state envelope.
package state

import (
	"context"
	"encoding/json"
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
	defaultIdleThreshold = 30 * time.Minute
	defaultRepoOpTimeout = 5 * time.Minute
	defaultPushAfter     = 24 * time.Hour
	stateIdentity        = "repo-sync-state-v1"
	stateNamespace       = "repo_sync"
	stateDeclaration     = "schema:{identity:string,version:uint64,fingerprint:string};host_registry:{self:string,hosts:array<string>,addrs:map<string,array<string>>};repo_sync:{default_location:string,repos:map<string,{added_at:int64,removed_at:int64,value:{relpath:string,trunk:string,local_only:bool,no_env_sync:bool}}>,local_repos:map<string,{added_at:int64,removed_at:int64,value:{relpath:string,trunk:string,local_only:bool,no_env_sync:bool}}>,settings:{idle_threshold:duration,repo_op_timeout:duration,push_after:duration}}"
)

// Config is reposync's host-registry handle, naming the tool so hostregistry
// resolves the config dir and the ssh probes. State and host both drive it.
var Config = hostregistry.Config{Name: ToolName, State: hostregistry.StateContract{
	Identity:         stateIdentity,
	Fingerprint:      hostregistry.SchemaFingerprint(stateIdentity, stateDeclaration),
	ProductNamespace: stateNamespace,
	InitialProduct:   mustEncodeProduct(New()),
	ValidateProduct:  validateProduct,
}}

// ErrLockBusy is returned when the reconcile lock is held past the caller's deadline.
var ErrLockBusy = hostregistry.ErrLockBusy

// Duration is the canonical Go-duration string codec, re-exported from
// github.com/yasyf/synckit/codec so callers in this module spell it state.Duration.
type Duration = codec.Duration

// Settings holds the cadence knobs read by the sync and reconcile loops.
type Settings struct {
	IdleThreshold codec.Duration `json:"idle_threshold"`
	RepoOpTimeout codec.Duration `json:"repo_op_timeout"`
	PushAfter     codec.Duration `json:"push_after"`
}

// RepoMeta is the per-repo payload carried by the convergent registry: where the
// repo lives (Relpath under the default location), its Trunk branch, whether it is
// local-only, and whether it opts out of env-file sync. The registry key carries the
// repo's identity — its origin for a propagating repo, its relpath for a local-only
// one — so it is absent from this payload. NoEnvSync's zero value keeps env sync on.
type RepoMeta struct {
	Relpath   string `json:"relpath"`
	Trunk     string `json:"trunk"`
	LocalOnly bool   `json:"local_only"`
	NoEnvSync bool   `json:"no_env_sync"`
}

// Repo is a tracked repository placed at Relpath under the host's default location.
// It is the in-memory view the sync, reconcile, and watch loops consume, rebuilt
// from a registry [RepoMeta] plus its key; Origin is empty for a local-only repo.
// NoEnvSync opts the repo out of env-file sync; its zero value keeps env sync on.
type Repo struct {
	Relpath   string
	Origin    string
	Trunk     string
	LocalOnly bool
	NoEnvSync bool
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

type stateJSON struct {
	DefaultLocation string                       `json:"default_location"`
	Repos           cregistry.Registry[RepoMeta] `json:"repos"`
	LocalRepos      cregistry.Registry[RepoMeta] `json:"local_repos"`
	Settings        Settings                     `json:"settings"`
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
	return Repo{Relpath: e.Value.Relpath, Origin: origin, Trunk: e.Value.Trunk, LocalOnly: e.Value.LocalOnly, NoEnvSync: e.Value.NoEnvSync}
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
	meta := RepoMeta{Relpath: r.Relpath, Trunk: r.Trunk, LocalOnly: r.LocalOnly, NoEnvSync: r.NoEnvSync}
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

// Save persists a complete exact repo-sync payload. It explicitly initializes
// an absent v1 envelope but never repairs an existing file.
func (s *State) Save() error {
	if err := Config.InitializeState(context.Background()); err != nil {
		return err
	}
	return Config.UpdateProduct(context.Background(), func(json.RawMessage) (json.RawMessage, error) {
		return encodeProduct(s)
	})
}

// SaveReposUnlocked persists the propagating and local repo registries without
// acquiring the reconcile lock. It preserves the other declared fields in the
// exact repo_sync payload. The convergent-reconcile pass already holds the lock
// around the whole pass and must not re-enter the non-reentrant flock; ordinary
// callers use Update or Save. The flock is the caller's responsibility.
func (s *State) SaveReposUnlocked() error {
	return Config.UpdateProductUnlocked(func(raw json.RawMessage) (json.RawMessage, error) {
		persisted, err := decodeProduct(raw)
		if err != nil {
			return nil, err
		}
		persisted.Repos = s.Repos
		persisted.LocalRepos = s.LocalRepos
		return encodeProduct(persisted)
	})
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
	if s.Settings.IdleThreshold == 0 {
		s.Settings.IdleThreshold = Duration(defaultIdleThreshold)
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

// Load reads the exact schema v1 repo-sync payload.
func Load() (*State, error) {
	raw, err := Config.LoadProduct()
	if err != nil {
		return nil, err
	}
	return decodeProduct(raw)
}

// Update runs fn against a freshly loaded State under the reconcile-lock flock,
// then replaces the exact repo_sync payload while preserving the declared
// host_registry payload. It serializes read-modify-write across processes through
// the one canonical flock that hostregistry writers also hold.
func Update(ctx context.Context, fn func(*State) error) (*State, error) {
	var out *State
	err := Config.UpdateProduct(ctx, func(raw json.RawMessage) (json.RawMessage, error) {
		st, err := decodeProduct(raw)
		if err != nil {
			return nil, err
		}
		if err := fn(st); err != nil {
			return nil, err
		}
		out = st
		return encodeProduct(st)
	})
	return out, err
}

// Initialize creates a fresh complete v1 envelope when none exists.
func Initialize(ctx context.Context) error { return Config.InitializeState(ctx) }

func decodeProduct(raw json.RawMessage) (*State, error) {
	var persisted stateJSON
	if err := hostregistry.DecodeExactJSON(raw, &persisted); err != nil {
		return nil, fmt.Errorf("decode repo_sync: %w", err)
	}
	s := &State{DefaultLocation: persisted.DefaultLocation, Repos: persisted.Repos, LocalRepos: persisted.LocalRepos, Settings: persisted.Settings}
	if err := validateState(s); err != nil {
		return nil, err
	}
	return s, nil
}

func validateProduct(raw json.RawMessage) error {
	_, err := decodeProduct(raw)
	return err
}

func validateState(s *State) error {
	if s.DefaultLocation == "" || s.Repos == nil || s.LocalRepos == nil {
		return fmt.Errorf("repo_sync requires default_location, repos, and local_repos")
	}
	if s.Settings.IdleThreshold <= 0 || s.Settings.RepoOpTimeout <= 0 || s.Settings.PushAfter <= 0 {
		return fmt.Errorf("repo_sync settings durations must be positive")
	}
	for key, entry := range s.Repos {
		if key == "" || entry.Value.Relpath == "" || entry.Value.LocalOnly || (entry.Added <= 0 && entry.Removed <= 0) {
			return fmt.Errorf("invalid propagating repo entry %q", key)
		}
	}
	for key, entry := range s.LocalRepos {
		if key == "" || entry.Value.Relpath != key || !entry.Value.LocalOnly || (entry.Added <= 0 && entry.Removed <= 0) {
			return fmt.Errorf("invalid local repo entry %q", key)
		}
	}
	return nil
}

func encodeProduct(s *State) (json.RawMessage, error) {
	if err := validateState(s); err != nil {
		return nil, err
	}
	data, err := json.Marshal(stateJSON{DefaultLocation: s.DefaultLocation, Repos: s.Repos, LocalRepos: s.LocalRepos, Settings: s.Settings})
	if err != nil {
		return nil, fmt.Errorf("encode repo_sync: %w", err)
	}
	return data, nil
}

func mustEncodeProduct(s *State) json.RawMessage {
	raw, err := encodeProduct(s)
	if err != nil {
		panic(err)
	}
	return raw
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
