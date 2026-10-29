package cli

import (
	"context"
	"testing"

	"github.com/yasyf/daemonkit"
)

func TestOrdinaryCommandLeavesResidentHelperAlone(t *testing.T) {
	seedRegistry(t, "yasyf@laptop")
	calls := 0
	previous := ensureHelper
	ensureHelper = func(context.Context, string) (daemonkit.Ensured, error) {
		calls++
		return daemonkit.Ensured{}, nil
	}
	t.Cleanup(func() { ensureHelper = previous })

	if _, _, err := runCLI(t, "self", "--json"); err != nil {
		t.Fatalf("self --json: %v", err)
	}
	if calls != 0 {
		t.Fatalf("self --json ensured the resident helper %d times, want 0", calls)
	}
}
