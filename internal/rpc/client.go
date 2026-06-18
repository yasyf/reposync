package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
)

// Sync asks the daemon at sockPath to idle-sync the registered repos, optionally
// narrowed to relpath and tagged with the anti-echo origin.
func Sync(ctx context.Context, sockPath, relpath, origin string) (*Response, error) {
	return call(ctx, sockPath, Request{Method: MethodSync, Relpath: relpath, Origin: origin})
}

// Reconcile asks the daemon at sockPath to clone-and-sync every registered repo.
func Reconcile(ctx context.Context, sockPath string) (*Response, error) {
	return call(ctx, sockPath, Request{Method: MethodReconcile})
}

func call(ctx context.Context, sockPath string, req Request) (*Response, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial daemon at %s: %w", sockPath, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}
