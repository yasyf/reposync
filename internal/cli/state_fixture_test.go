package cli

import (
	"testing"

	"github.com/yasyf/reposync/internal/state"
	"github.com/yasyf/synckit/hostregistry"
)

func initializeProductState(t *testing.T) {
	t.Helper()
	if err := state.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize repo-sync state: %v", err)
	}
}

func initializeMeshState(t *testing.T) {
	t.Helper()
	if err := hostregistry.Mesh.InitializeState(t.Context()); err != nil {
		t.Fatalf("initialize mesh state: %v", err)
	}
}
