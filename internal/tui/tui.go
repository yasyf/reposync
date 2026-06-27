// Package tui wires reposync's Repos content screen into the shared synckit TUI,
// which supplies the tab router, header, and the built-in Hosts tab.
package tui

import (
	"context"

	"github.com/yasyf/synckit/hostregistry"
	synckittui "github.com/yasyf/synckit/tui"
)

// Run launches the interactive TUI: reposync's Repos screen plus the shared Hosts
// tab. It blocks until the user quits or ctx is canceled.
func Run(ctx context.Context, version string) error {
	return synckittui.Run(ctx, synckittui.Options{
		Brand:   "reposync",
		Version: version,
		Screens: []synckittui.Screen{newReposModel()},
		Runner:  hostregistry.NewExecRunner(),
	})
}
