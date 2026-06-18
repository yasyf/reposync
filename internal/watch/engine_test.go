package watch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/reposync/internal/state"
)

// fakeResolver returns scripted hashes per call, advancing through the script so
// successive evaluations can observe a changed or unchanged hash.
type fakeResolver struct {
	mu     sync.Mutex
	hashes []string
	err    error
	calls  int
}

func (r *fakeResolver) Resolve(_ context.Context, _ state.Repo) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	h := r.hashes[r.calls]
	if r.calls < len(r.hashes)-1 {
		r.calls++
	}
	return h, nil
}

// notifyCall records one (peer, relpath) notification.
type notifyCall struct {
	peer    string
	relpath string
}

// fakeNotifier records every notification and can be told to fail for one peer.
type fakeNotifier struct {
	mu       sync.Mutex
	calls    []notifyCall
	failPeer string
}

func (n *fakeNotifier) Notify(_ context.Context, peer string, repo state.Repo) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, notifyCall{peer: peer, relpath: repo.Relpath})
	if peer == n.failPeer {
		return errors.New("peer unreachable")
	}
	return nil
}

func (n *fakeNotifier) snapshot() []notifyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notifyCall, len(n.calls))
	copy(out, n.calls)
	return out
}

// fakeTimer is a debounce timer whose fire is triggered by the test, never by
// wall-clock time, so debounce coalescing is deterministic. resets counts how
// many times the timer was re-armed (one per coalesced event after the first).
type fakeTimer struct {
	mu     sync.Mutex
	fn     func()
	resets int
	popped bool
}

func (t *fakeTimer) Reset(time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resets++
	return true
}

func (t *fakeTimer) Stop() bool { return true }

// fire invokes the debounce callback, mimicking the timer expiring after the
// debounce window of quiescence.
func (t *fakeTimer) fire() {
	t.mu.Lock()
	if t.popped {
		t.mu.Unlock()
		return
	}
	t.popped = true
	fn := t.fn
	t.mu.Unlock()
	fn()
}

func (t *fakeTimer) resetCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resets
}

const testRelpath = "cc-review"

func testRepo() state.Repo {
	return state.Repo{Relpath: testRelpath, Origin: "https://github.com/yasyf/cc-review.git", Trunk: "main"}
}

func TestDebounceCoalescesBurstIntoOneEvaluation(t *testing.T) {
	resolver := &fakeResolver{hashes: []string{"hashA"}}
	notifier := &fakeNotifier{}
	eng := newEngine(resolver, notifier, time.Hour, []string{"peer1"})

	var ft *fakeTimer
	eng.newTimer = func(_ time.Duration, fn func()) timer {
		ft = &fakeTimer{fn: fn}
		return ft
	}

	repo := testRepo()
	ctx := context.Background()
	eng.onEvent(ctx, repo)
	eng.onEvent(ctx, repo)
	eng.onEvent(ctx, repo)

	if ft == nil {
		t.Fatal("no timer was armed")
	}
	if got := ft.resetCount(); got != 2 {
		t.Errorf("timer resets = %d, want 2 (3 events, 1 arm + 2 resets)", got)
	}

	resolver.mu.Lock()
	calls := resolver.calls
	resolver.mu.Unlock()
	if calls != 0 || len(notifier.snapshot()) != 0 {
		t.Fatalf("evaluation ran before debounce fired: resolver.calls=%d notifies=%d", calls, len(notifier.snapshot()))
	}

	ft.fire()

	if got := len(notifier.snapshot()); got != 1 {
		t.Fatalf("notifies = %d, want exactly 1 (burst coalesced)", got)
	}
	if got := notifier.snapshot()[0]; got.peer != "peer1" || got.relpath != testRelpath {
		t.Errorf("notify = %+v, want {peer1 %s}", got, testRelpath)
	}
}

func TestDedupeUnchangedHashNoNotifyOnSecondEvaluation(t *testing.T) {
	resolver := &fakeResolver{hashes: []string{"hashA", "hashA"}}
	notifier := &fakeNotifier{}
	eng := newEngine(resolver, notifier, time.Hour, []string{"peer1"})
	ctx := context.Background()
	repo := testRepo()

	eng.evaluate(ctx, repo)
	eng.evaluate(ctx, repo)

	if got := len(notifier.snapshot()); got != 1 {
		t.Fatalf("notifies = %d, want 1 (second evaluation deduped)", got)
	}
}

func TestHashChangeNotifiesAllPeersOnceAndUpdatesLastHash(t *testing.T) {
	resolver := &fakeResolver{hashes: []string{"hashA", "hashB"}}
	notifier := &fakeNotifier{}
	hosts := []string{"peer1", "peer2", "peer3"}
	eng := newEngine(resolver, notifier, time.Hour, hosts)
	ctx := context.Background()
	repo := testRepo()

	eng.evaluate(ctx, repo) // hashA -> notify all
	eng.evaluate(ctx, repo) // hashB -> notify all again

	calls := notifier.snapshot()
	if len(calls) != 6 {
		t.Fatalf("notifies = %d, want 6 (3 peers x 2 changes)", len(calls))
	}
	perPeer := map[string]int{}
	for _, c := range calls {
		if c.relpath != testRelpath {
			t.Errorf("notify relpath = %q, want %q", c.relpath, testRelpath)
		}
		perPeer[c.peer]++
	}
	for _, peer := range hosts {
		if perPeer[peer] != 2 {
			t.Errorf("peer %s notified %d times, want 2", peer, perPeer[peer])
		}
	}

	eng.mu.Lock()
	got := eng.lastHash[testRelpath]
	eng.mu.Unlock()
	if got != "hashB" {
		t.Errorf("lastHash = %q, want hashB", got)
	}
}

func TestResolverErrorNoNotifyNoCrash(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("no trunk")}
	notifier := &fakeNotifier{}
	eng := newEngine(resolver, notifier, time.Hour, []string{"peer1"})

	eng.evaluate(context.Background(), testRepo())

	if got := len(notifier.snapshot()); got != 0 {
		t.Errorf("notifies = %d, want 0 on resolver error", got)
	}
	eng.mu.Lock()
	_, recorded := eng.lastHash[testRelpath]
	eng.mu.Unlock()
	if recorded {
		t.Error("lastHash recorded despite resolver error")
	}
}

func TestOnePeerFailureDoesNotBlockOthers(t *testing.T) {
	resolver := &fakeResolver{hashes: []string{"hashA"}}
	notifier := &fakeNotifier{failPeer: "peer2"}
	hosts := []string{"peer1", "peer2", "peer3"}
	eng := newEngine(resolver, notifier, time.Hour, hosts)

	eng.evaluate(context.Background(), testRepo())

	calls := notifier.snapshot()
	if len(calls) != 3 {
		t.Fatalf("notifies = %d, want 3 (all peers attempted despite one failure)", len(calls))
	}
	notified := map[string]bool{}
	for _, c := range calls {
		notified[c.peer] = true
	}
	for _, peer := range hosts {
		if !notified[peer] {
			t.Errorf("peer %s was not notified (failure isolation broken)", peer)
		}
	}
}

func TestAntiEchoSameHashTerminatesLoop(t *testing.T) {
	// First event resolves X and notifies; a second event (the self-induced
	// fetch echo) resolves the SAME X and must produce no further notify.
	resolver := &fakeResolver{hashes: []string{"hashX", "hashX"}}
	notifier := &fakeNotifier{}
	eng := newEngine(resolver, notifier, time.Hour, []string{"peer1", "peer2"})
	ctx := context.Background()
	repo := testRepo()

	eng.evaluate(ctx, repo) // resolves X, notifies both peers
	if got := len(notifier.snapshot()); got != 2 {
		t.Fatalf("first evaluate notifies = %d, want 2", got)
	}

	eng.evaluate(ctx, repo) // echo: same X, no notify
	if got := len(notifier.snapshot()); got != 2 {
		t.Fatalf("after echo notifies = %d, want 2 (loop terminated, no new notify)", got)
	}
}

func TestNoPeersNoNotifyButLastHashTracked(t *testing.T) {
	resolver := &fakeResolver{hashes: []string{"hashA"}}
	notifier := &fakeNotifier{}
	eng := newEngine(resolver, notifier, time.Hour, nil)

	eng.evaluate(context.Background(), testRepo())

	if got := len(notifier.snapshot()); got != 0 {
		t.Errorf("notifies = %d, want 0 with no peers", got)
	}
	eng.mu.Lock()
	got := eng.lastHash[testRelpath]
	eng.mu.Unlock()
	if got != "hashA" {
		t.Errorf("lastHash = %q, want hashA tracked even with no peers", got)
	}
}
