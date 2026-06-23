package rpc

import (
	"context"
	"net"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
	syncpkg "github.com/yasyf/reposync/internal/sync"
	synckit "github.com/yasyf/synckit/rpc"
)

// Server handles RPC requests against the host's reposync state. It wraps a synckit
// Dispatcher, which serializes dispatch so the per-host flock is acquired by at most
// one in-flight request at a time, never nested.
type Server struct {
	dispatcher *synckit.Dispatcher
}

// NewServer returns a Server whose handlers load fresh state via load for each
// request, registering the sync and reconcile methods on a synckit Dispatcher.
func NewServer(load func() (*state.State, error)) *Server {
	d := synckit.NewDispatcher()
	d.Register(string(MethodSync), syncHandler(load))
	d.Register(string(MethodReconcile), reconcileHandler(load))
	return &Server{dispatcher: d}
}

// Serve accepts and handles one request per connection until ctx is canceled, then
// returns nil. It does not close ln; the caller owns the listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return synckit.Serve(ctx, ln, s.dispatcher)
}

func syncHandler(load func() (*state.State, error)) synckit.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		st, err := load()
		if err != nil {
			return nil, err
		}
		var res []syncpkg.Result
		err = state.WithLock(ctx, func() error {
			r, err := syncpkg.Sync(ctx, st, stringParam(params, paramRelpath), stringParam(params, paramOrigin))
			res = r
			return err
		})
		if err != nil {
			return nil, err
		}
		return resultsFromSync(res), nil
	}
}

func reconcileHandler(load func() (*state.State, error)) synckit.Handler {
	return func(ctx context.Context, _ map[string]any) (any, error) {
		st, err := load()
		if err != nil {
			return nil, err
		}
		res, err := reconcile.Reconcile(ctx, st)
		if err != nil {
			return nil, err
		}
		return resultsFromReconcile(res), nil
	}
}

func stringParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}
