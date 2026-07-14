package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/vcs"
	"github.com/yasyf/reposync/internal/vcs/vcstest"
)

// TestWatchItemBusyOnLockMarker proves watchItem carries the live-operation probe
// onto the wire: a lock marker in the checkout reports Busy with the exact reason,
// and the item reads idle again once the marker is gone.
func TestWatchItemBusyOnLockMarker(t *testing.T) {
	f := vcstest.New(t)
	dl := filepath.Join(f.Root, "data")
	dest := f.GitClone(filepath.Join(dl, "alpha"))
	repo := state.Repo{Relpath: "alpha", Trunk: "main"}

	var errw bytes.Buffer
	item := watchItem(t.Context(), &errw, "alpha", repo, dl)
	if item.Busy || item.BusyReason != "" {
		t.Fatalf("idle item = (busy=%v, reason=%q), want idle", item.Busy, item.BusyReason)
	}

	lock := filepath.Join(dest, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	item = watchItem(t.Context(), &errw, "alpha", repo, dl)
	if !item.Busy {
		t.Fatal("locked item not busy")
	}
	if item.BusyReason != "git index locked" {
		t.Fatalf("busy reason = %q, want git index locked", item.BusyReason)
	}
	// A busy repo still carries its fingerprint and watch dirs — busy defers, never drops.
	if item.Fingerprint == "" || len(item.WatchDirs) == 0 {
		t.Fatalf("busy item lost fingerprint or watch dirs: %+v", item)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	item = watchItem(t.Context(), &errw, "alpha", repo, dl)
	if item.Busy || item.BusyReason != "" {
		t.Fatalf("post-cleanup item = (busy=%v, reason=%q), want idle", item.Busy, item.BusyReason)
	}
	if errw.Len() != 0 {
		t.Fatalf("probe logged unexpectedly: %s", errw.String())
	}
}

// TestWatchItemAddsUntrackedEnvPaths proves an eligible repo watches every
// untracked env file after its VCS paths while retaining each static candidate once.
func TestWatchItemAddsUntrackedEnvPaths(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	alpha := fx.GitClone(filepath.Join(dl, "alpha"))
	fx.WriteFile(alpha, ".env", "KEY=value\n")
	fx.WriteFile(alpha, ".env.dev", "DEV_KEY=value\n")

	var errw bytes.Buffer
	item := watchItem(t.Context(), &errw, fx.Origin, state.Repo{
		Relpath: "alpha",
		Origin:  fx.Origin,
		Trunk:   "main",
	}, dl)
	want := append(vcs.WatchPaths(alpha),
		filepath.Join(alpha, ".env"),
		filepath.Join(alpha, ".env.dev"),
		filepath.Join(alpha, ".env.local"),
	)
	if !slices.Equal(item.WatchDirs, want) {
		t.Fatalf("watch dirs = %v, want %v", item.WatchDirs, want)
	}
	if errw.Len() != 0 {
		t.Fatalf("watch item logged unexpectedly: %s", errw.String())
	}
}

// TestWatchItemIneligibleReposOmitEnv proves both env opt-out forms retain only
// VCS watch paths and the uniform fingerprint carries an empty env component.
func TestWatchItemIneligibleReposOmitEnv(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	alpha := fx.GitClone(filepath.Join(dl, "alpha"))
	fx.WriteFile(alpha, ".env", "KEY=value\n")

	cases := []struct {
		name string
		repo state.Repo
	}{
		{
			name: "env sync disabled",
			repo: state.Repo{Relpath: "alpha", Origin: fx.Origin, Trunk: "main", NoEnvSync: true},
		},
		{
			name: "local only",
			repo: state.Repo{Relpath: "alpha", Trunk: "main", LocalOnly: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errw bytes.Buffer
			item := watchItem(t.Context(), &errw, tc.name, tc.repo, dl)
			if want := vcs.WatchPaths(alpha); !slices.Equal(item.WatchDirs, want) {
				t.Fatalf("watch dirs = %v, want %v", item.WatchDirs, want)
			}
			if want := fx.OriginMain() + "\n"; item.Fingerprint != want {
				t.Fatalf("fingerprint = %q, want %q", item.Fingerprint, want)
			}
			if errw.Len() != 0 {
				t.Fatalf("watch item logged unexpectedly: %s", errw.String())
			}
		})
	}
}

// TestWatchItemFingerprintTracksEnvValues proves comments do not affect the
// values-only env digest while changing a value moves the fingerprint.
func TestWatchItemFingerprintTracksEnvValues(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	alpha := fx.GitClone(filepath.Join(dl, "alpha"))
	repo := state.Repo{Relpath: "alpha", Origin: fx.Origin, Trunk: "main"}

	var errw bytes.Buffer
	fx.WriteFile(alpha, ".env", "KEY=a\n")
	initial := watchItem(t.Context(), &errw, fx.Origin, repo, dl).Fingerprint
	fx.WriteFile(alpha, ".env", "KEY=a\n# comment only\n")
	commented := watchItem(t.Context(), &errw, fx.Origin, repo, dl).Fingerprint
	if commented != initial {
		t.Fatalf("comment-only edit moved fingerprint: before %q, after %q", initial, commented)
	}

	fx.WriteFile(alpha, ".env", "KEY=b\n")
	changed := watchItem(t.Context(), &errw, fx.Origin, repo, dl).Fingerprint
	if changed == initial {
		t.Fatalf("value edit did not move fingerprint: before %q, after %q", initial, changed)
	}
	if errw.Len() != 0 {
		t.Fatalf("watch item logged unexpectedly: %s", errw.String())
	}
}

// TestWatchItemTrackedEnvContributesNoState proves a staged env file remains only
// as a static watch candidate and contributes no values to the eligible digest.
func TestWatchItemTrackedEnvContributesNoState(t *testing.T) {
	fx := vcstest.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dl := filepath.Join(fx.Root, "data")
	alpha := fx.GitClone(filepath.Join(dl, "alpha"))
	repo := state.Repo{Relpath: "alpha", Origin: fx.Origin, Trunk: "main"}

	var errw bytes.Buffer
	withoutEnv := watchItem(t.Context(), &errw, fx.Origin, repo, dl)
	fx.WriteFile(alpha, ".env", "TRACKED=value\n")
	fx.RunGit(alpha, "add", ".env")
	tracked := watchItem(t.Context(), &errw, fx.Origin, repo, dl)

	wantPaths := append(vcs.WatchPaths(alpha),
		filepath.Join(alpha, ".env"),
		filepath.Join(alpha, ".env.local"),
	)
	if !slices.Equal(tracked.WatchDirs, wantPaths) {
		t.Fatalf("watch dirs = %v, want %v", tracked.WatchDirs, wantPaths)
	}
	if tracked.Fingerprint != withoutEnv.Fingerprint {
		t.Fatalf("tracked .env moved fingerprint: before %q, after %q", withoutEnv.Fingerprint, tracked.Fingerprint)
	}
	if errw.Len() != 0 {
		t.Fatalf("watch item logged unexpectedly: %s", errw.String())
	}
}
