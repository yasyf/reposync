package watch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yasyf/reposync/internal/state"
)

// recordingRunner is a peerRunner that records every ssh command per target and
// returns a fixed error, so notifier tests can assert the exact remote command.
type recordingRunner struct {
	mu   sync.Mutex
	cmds map[string][]string
	err  error
}

func (r *recordingRunner) SSH(ctx context.Context, target, remoteCmd string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmds == nil {
		r.cmds = map[string][]string{}
	}
	r.cmds[target] = append(r.cmds[target], remoteCmd)
	return "", r.err
}

func TestRPCNotifierIssuesSyncCommand(t *testing.T) {
	rec := &recordingRunner{}
	n := rpcNotifier{self: "yasyf@self", runner: rec}

	if err := n.Notify(context.Background(), "yasyf@peer", state.Repo{Relpath: "Forge/private-ai"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got := rec.cmds["yasyf@peer"]
	want := []string{"reposync rpc sync --relpath 'Forge/private-ai' --origin 'yasyf@self'"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ssh cmds = %v, want %v", got, want)
	}
}

func TestRPCNotifierQuotesRelpathWithSpaces(t *testing.T) {
	rec := &recordingRunner{}
	n := rpcNotifier{self: "yasyf@self", runner: rec}

	if err := n.Notify(context.Background(), "yasyf@peer", state.Repo{Relpath: "Forge/my repo"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got := rec.cmds["yasyf@peer"]
	want := []string{"reposync rpc sync --relpath 'Forge/my repo' --origin 'yasyf@self'"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ssh cmds = %v, want %v", got, want)
	}
}

func TestRPCNotifierPropagatesSSHError(t *testing.T) {
	rec := &recordingRunner{err: errors.New("connection refused")}
	n := rpcNotifier{self: "self", runner: rec}

	err := n.Notify(context.Background(), "peer", state.Repo{Relpath: "r"})
	if err == nil {
		t.Fatal("expected an error when ssh fails")
	}
	if !errors.Is(err, rec.err) {
		t.Fatalf("error %v should wrap the ssh error", err)
	}
}
