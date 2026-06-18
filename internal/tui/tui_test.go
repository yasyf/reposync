package tui

import (
	"bytes"
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/yasyf/reposync/internal/state"
)

// fakeRunner satisfies host.Runner with canned, network-free replies: Local
// answers the tailscale and id probes host discovery makes; SSH always succeeds
// silently so verify never reaches the network.
type fakeRunner struct{}

const fakeTailscaleJSON = `{
  "Self": {"DNSName": "self.tailnet.ts.net.", "HostName": "self", "Online": true},
  "Peer": {
    "key-alpha": {"DNSName": "alpha.tailnet.ts.net.", "HostName": "alpha", "Online": true,  "OS": "linux"},
    "key-beta":  {"DNSName": "beta.tailnet.ts.net.",  "HostName": "beta",  "Online": false, "OS": "macOS"}
  }
}`

func (fakeRunner) Local(_ context.Context, name string, _ ...string) (string, error) {
	switch name {
	case "tailscale":
		return fakeTailscaleJSON, nil
	case "id":
		return "yasyf\n", nil
	}
	return "", nil
}

func (fakeRunner) SSH(_ context.Context, _, _ string) (string, error) { return "", nil }

// hermeticOptions points reposync's state at a fresh temp config dir whose
// default_location is an empty directory, so repo discovery returns zero
// candidates without scanning the real filesystem, and wires in the fake runner.
func hermeticOptions(t *testing.T) Options {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	emptyRepos := t.TempDir()
	if _, err := state.Update(func(s *state.State) error {
		s.DefaultLocation = emptyRepos
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	return Options{Version: "test", Runner: fakeRunner{}}
}

// waitForContent fails the test unless every substr appears in the model output.
// WaitFor drains the shared output buffer as it reads, so content that renders
// only once must be asserted in a single call: chaining one WaitFor per
// substring would make later calls block on frames that already scrolled past.
func waitForContent(t *testing.T, tm *teatest.TestModel, substrs ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, s := range substrs {
			if !bytes.Contains(b, []byte(s)) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(20*time.Millisecond))
}

func TestTabBarAndSwitch(t *testing.T) {
	opts := hermeticOptions(t)
	tm := teatest.NewTestModel(t, newRootModel(opts), teatest.WithInitialTermSize(100, 30))

	// The tab bar names both screens (each tab label is styled, so assert the bare
	// words), and the repos screen settles on its empty state — all in one
	// accumulated read, since the empty state renders only once.
	waitForContent(t, tm, "Repos", "Hosts", "No git/jj repos found")

	// NextTab ("n") activates the Hosts screen, which lazily initializes and shows
	// its discovering state. The spinner keeps re-rendering that line while the
	// scan runs, so it recurs in fresh frames after the tab switch.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	waitForContent(t, tm, "Discovering hosts")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
}

func TestAddHostInputAndCancel(t *testing.T) {
	opts := hermeticOptions(t)
	tm := teatest.NewTestModel(t, newRootModel(opts), teatest.WithInitialTermSize(100, 30))

	// Switch to the Hosts tab and let it reach its discovering state.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	waitForContent(t, tm, "Discovering hosts")

	// "+" opens the add-host text input, which surfaces its prompt and placeholder
	// in the same frame.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'+'}})
	waitForContent(t, tm, "Add host:", "user@node")

	// esc returns to the list — the add-host prompt gives way to the list chrome
	// whose help footer offers "add host" again.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	waitForContent(t, tm, "add host")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
}

func TestCtrlCQuitsCleanly(t *testing.T) {
	opts := hermeticOptions(t)
	tm := teatest.NewTestModel(t, newRootModel(opts), teatest.WithInitialTermSize(100, 30))

	// Let the first frame render so the program is past Init before quitting.
	waitForContent(t, tm, "Repos")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

	// The router blanks its view on quit; the final model retains the quit flag.
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(rootModel)
	if !final.quitting {
		t.Fatal("rootModel.quitting = false after ctrl+c, want true")
	}
}

func TestReposEmptyState(t *testing.T) {
	opts := hermeticOptions(t)
	tm := teatest.NewTestModel(t, newRootModel(opts), teatest.WithInitialTermSize(100, 30))

	// default_location is an empty dir, so the repos screen lands on its empty state.
	waitForContent(t, tm, "No git/jj repos found")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
}
