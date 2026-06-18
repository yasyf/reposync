package watch

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/yasyf/reposync/internal/state"
)

// Resolver resolves the truth of a repo's origin trunk: the commit hash of
// origin/<trunk>. It must never read a ref file directly (packed-refs can
// shadow a loose ref), so the default implementation shells out to git rev-parse.
type Resolver interface {
	Resolve(ctx context.Context, repo state.Repo) (string, error)
}

// Notifier tells one peer host to run a fast single-repo sync. A failure to
// reach one peer must not block the others.
type Notifier interface {
	Notify(ctx context.Context, peer string, repo state.Repo) error
}

// timer is the slice of *time.Timer the engine depends on, extracted so tests
// can drive debounce deterministically instead of sleeping.
type timer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

// newTimer builds a debounce timer that fires fn after d. The production
// implementation is a real *time.Timer; tests swap in a fake.
type newTimerFunc func(d time.Duration, fn func()) timer

// realTimer adapts *time.Timer to the timer interface.
type realTimer struct{ t *time.Timer }

func (rt realTimer) Reset(d time.Duration) bool { return rt.t.Reset(d) }
func (rt realTimer) Stop() bool                 { return rt.t.Stop() }

func realNewTimer(d time.Duration, fn func()) timer {
	return realTimer{t: time.AfterFunc(d, fn)}
}

// engine is the pure debounce + dedupe + notify core, free of any watchman or
// ssh wiring. onEvent coalesces a burst of events per repo into a single
// evaluate; evaluate resolves trunk truth, dedupes by hash, and fans out to
// every peer. The boundaries (Resolver, Notifier, newTimer) are injected so the
// whole core is driven directly in tests.
type engine struct {
	resolver Resolver
	notifier Notifier
	debounce time.Duration
	hosts    []string
	newTimer newTimerFunc

	mu       sync.Mutex
	timers   map[string]timer  // per-repo debounce timer, keyed by relpath
	lastHash map[string]string // per-repo last-acted-on origin/<trunk> hash, keyed by relpath
}

func newEngine(resolver Resolver, notifier Notifier, debounce time.Duration, hosts []string) *engine {
	return &engine{
		resolver: resolver,
		notifier: notifier,
		debounce: debounce,
		hosts:    hosts,
		newTimer: realNewTimer,
		timers:   make(map[string]timer),
		lastHash: make(map[string]string),
	}
}

// onEvent records a filesystem event for repo. It (re)arms a single per-repo
// debounce timer so a burst of events — one fetch writes a loose ref,
// FETCH_HEAD, and an op head — collapses into exactly one evaluate once the
// repo has been quiet for the debounce window.
func (e *engine) onEvent(ctx context.Context, repo state.Repo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.timers[repo.Relpath]; ok {
		t.Reset(e.debounce)
		return
	}
	e.timers[repo.Relpath] = e.newTimer(e.debounce, func() {
		e.fire(ctx, repo)
	})
}

// fire drops the spent timer and runs the evaluation for repo.
func (e *engine) fire(ctx context.Context, repo state.Repo) {
	e.mu.Lock()
	delete(e.timers, repo.Relpath)
	e.mu.Unlock()
	e.evaluate(ctx, repo)
}

// evaluate resolves the origin trunk hash, dedupes against the last acted-on
// hash, and on a real change records the new hash *before* notifying peers (so
// the self-induced fetch event that follows is recognized as a no-op) then
// notifies every peer concurrently. A resolver error (no origin, no trunk) is
// logged and skipped silently; it never crashes or notifies.
func (e *engine) evaluate(ctx context.Context, repo state.Repo) {
	hash, err := e.resolver.Resolve(ctx, repo)
	if err != nil {
		log.Printf("watch: %s: resolve trunk: %v", repo.Relpath, err)
		return
	}

	e.mu.Lock()
	if e.lastHash[repo.Relpath] == hash {
		e.mu.Unlock()
		return
	}
	e.lastHash[repo.Relpath] = hash
	e.mu.Unlock()

	e.notifyPeers(ctx, repo)
}

// notifyPeers fans the notification out to every peer concurrently. A down or
// failing peer is logged and isolated — the others are still notified.
func (e *engine) notifyPeers(ctx context.Context, repo state.Repo) {
	var wg sync.WaitGroup
	for _, peer := range e.hosts {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			if err := e.notifier.Notify(ctx, peer, repo); err != nil {
				log.Printf("watch: %s: notify %s: %v", repo.Relpath, peer, err)
			}
		}(peer)
	}
	wg.Wait()
}
