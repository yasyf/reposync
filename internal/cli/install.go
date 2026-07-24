package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/synckit/codec"
	"github.com/yasyf/synckit/hostregistry"
	"github.com/yasyf/synckit/manifest"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/reposync/internal/transfer"
)

// manifestsDirName is the subdirectory of the shared synckit config dir where
// consumers register their manifests for synckitd to discover.
const manifestsDirName = "manifests"

// watchDebounce is the quiet window synckitd waits after a repo's VCS metadata
// changes before triggering a converge: long enough to coalesce a burst of
// fetch writes and to outlast an interactive push, so the converge fires after
// it instead of mid-flight.
const watchDebounce = 15 * time.Second

// reposyncManifest is the declarative registration synckitd reads to drive reposync:
// the watch debounce and resident revisioned service it uses to reach reposync's
// typed sync contract.
func reposyncManifest() manifest.Manifest {
	return manifest.Manifest{
		Name:   state.ToolName,
		Binary: state.ToolName,
		Brew:   "yasyf/tap/reposync",
		Watch: manifest.WatchSpec{
			Debounce: codec.Duration(watchDebounce),
		},
		Service: manifest.ServiceSpec{
			Kind: "resident", Socket: "~/.config/reposync/rpc.sock",
			SchemaFingerprint: transfer.Fingerprint,
		},
		Helper: &manifest.HelperSpec{Command: "rpc-serve-v1", SessionType: manifest.SessionTypeBackground},
	}
}

// manifestPath returns the absolute path reposync's manifest registers under in the
// shared synckit config dir.
func manifestPath() (string, error) {
	dir, err := hostregistry.Mesh.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, manifestsDirName, state.ToolName+".json"), nil
}

func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register reposync's synckitd manifest so the daemon drives its sync.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := state.Initialize(cmd.Context()); err != nil {
				return fmt.Errorf("initialize repo-sync state: %w", err)
			}
			m := reposyncManifest()
			if err := m.Validate(); err != nil {
				return err
			}
			data, err := json.Marshal(m)
			if err != nil {
				return fmt.Errorf("encode manifest: %w", err)
			}
			path, err := manifestPath()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("create manifests dir %s: %w", filepath.Dir(path), err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				return fmt.Errorf("write manifest %s: %w", path, err)
			}
			cmd.Printf("registered reposync manifest %s\n", path)
			return nil
		},
	}
	return cmd
}

func newUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove reposync's synckitd manifest.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := manifestPath()
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove manifest %s: %w", path, err)
			}
			cmd.Printf("removed reposync manifest %s\n", path)
			return nil
		},
	}
	return cmd
}
