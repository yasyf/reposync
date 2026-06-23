package rpc

import (
	"context"

	synckit "github.com/yasyf/synckit/rpc"
)

// Sync asks the daemon at sockPath to idle-sync the registered repos, optionally
// narrowed to relpath and tagged with the anti-echo origin.
func Sync(ctx context.Context, sockPath, relpath, origin string) (*Response, error) {
	return call(ctx, sockPath, &synckit.Request{
		Method: string(MethodSync),
		Params: map[string]any{paramRelpath: relpath, paramOrigin: origin},
	})
}

// Reconcile asks the daemon at sockPath to clone-and-sync every registered repo.
func Reconcile(ctx context.Context, sockPath string) (*Response, error) {
	return call(ctx, sockPath, &synckit.Request{Method: string(MethodReconcile), Params: map[string]any{}})
}

func call(ctx context.Context, sockPath string, req *synckit.Request) (*Response, error) {
	resp, err := synckit.Call(ctx, sockPath, req)
	if err != nil {
		return nil, err
	}
	return responseFrom(resp)
}
