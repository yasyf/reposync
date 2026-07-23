package registry

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
)

func TestLoad(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(*testing.T) Registry
		wantErr bool
	}{
		{
			name: "missing state file returns an empty registry",
			seed: func(*testing.T) Registry {
				return Registry{}
			},
		},
		{
			name: "tracked local-only and tombstoned repos are filtered and sorted",
			seed: func(t *testing.T) Registry {
				defaultLocation := filepath.Join(t.TempDir(), "repos")
				seedState(t, defaultLocation,
					state.Repo{Relpath: "zeta", Origin: "https://example.com/zeta.git", Trunk: "main", NoEnvSync: true},
					state.Repo{Relpath: "alpha", Trunk: "trunk", LocalOnly: true},
					state.Repo{Relpath: "middle", Origin: "https://example.com/middle.git", Trunk: "master"},
				)
				removeRepo(t, "middle")
				return Registry{
					DefaultLocation: defaultLocation,
					Repos: []Repo{
						{Relpath: "alpha", Path: filepath.Join(defaultLocation, "alpha"), Trunk: "trunk", LocalOnly: true},
						{Relpath: "zeta", Path: filepath.Join(defaultLocation, "zeta"), Origin: "https://example.com/zeta.git", Trunk: "main", NoEnvSync: true},
					},
				}
			},
		},
		{
			name: "local-only repo has no origin",
			seed: func(t *testing.T) Registry {
				defaultLocation := filepath.Join(t.TempDir(), "repos")
				seedState(t, defaultLocation, state.Repo{Relpath: "scratch", Trunk: "main", LocalOnly: true, NoEnvSync: true})
				return Registry{
					DefaultLocation: defaultLocation,
					Repos: []Repo{{
						Relpath:   "scratch",
						Path:      filepath.Join(defaultLocation, "scratch"),
						Trunk:     "main",
						LocalOnly: true,
						NoEnvSync: true,
					}},
				}
			},
		},
		{
			name: "tilde default location is expanded in registry and repo paths",
			seed: func(t *testing.T) Registry {
				home := t.TempDir()
				t.Setenv("HOME", home)
				seedState(t, "~/Work", state.Repo{Relpath: "project", Origin: "https://example.com/project.git", Trunk: "main"})
				defaultLocation := filepath.Join(home, "Work")
				return Registry{
					DefaultLocation: defaultLocation,
					Repos: []Repo{{
						Relpath: "project",
						Path:    filepath.Join(defaultLocation, "project"),
						Origin:  "https://example.com/project.git",
						Trunk:   "main",
					}},
				}
			},
		},
		{
			name: "corrupt state JSON returns an error",
			seed: func(t *testing.T) Registry {
				path, err := state.Path()
				if err != nil {
					t.Fatalf("state path: %v", err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("mkdir state dir: %v", err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatalf("write corrupt state: %v", err)
				}
				return Registry{}
			},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			want := c.seed(t)

			got, err := Load()
			if c.wantErr {
				if err == nil {
					t.Fatal("Load: want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Load = %+v, want %+v", got, want)
			}
		})
	}
}

func seedState(t *testing.T, defaultLocation string, repos ...state.Repo) {
	t.Helper()
	if err := state.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize state: %v", err)
	}
	_, err := state.Update(context.Background(), func(st *state.State) error {
		st.DefaultLocation = defaultLocation
		for _, repo := range repos {
			st.AddRepo(repo)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func removeRepo(t *testing.T, relpath string) {
	t.Helper()
	originalNow := state.Now
	state.Now = func() time.Time { return originalNow().Add(time.Second) }
	t.Cleanup(func() { state.Now = originalNow })

	_, err := state.Update(context.Background(), func(st *state.State) error {
		st.RemoveRepo(relpath)
		return nil
	})
	if err != nil {
		t.Fatalf("remove repo: %v", err)
	}
}
