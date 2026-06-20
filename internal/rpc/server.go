package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
	syncpkg "github.com/yasyf/reposync/internal/sync"
)

// requestReadTimeout bounds how long a connection may take to deliver its request
// line, so a peer that connects and never writes cannot park a handler goroutine.
const requestReadTimeout = 30 * time.Second

// Server handles RPC requests against the host's reposync state. It serializes
// dispatch with a mutex so the per-host flock is acquired by at most one in-flight
// request at a time, never nested.
type Server struct {
	mu   sync.Mutex
	load func() (*state.State, error)
}

// NewServer returns a Server that loads fresh state via load for each request.
func NewServer(load func() (*state.State, error)) *Server {
	return &Server{load: load}
}

// Serve accepts and handles one request per connection until ctx is canceled,
// then returns nil. It does not close ln; the caller owns the listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept rpc connection: %w", err)
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(requestReadTimeout)); err != nil {
		return
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		writeResponse(conn, Response{Err: fmt.Sprintf("read request: %v", err)})
		return
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear; dispatch may run long
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeResponse(conn, Response{Err: fmt.Sprintf("decode request: %v", err)})
		return
	}
	writeResponse(conn, s.dispatch(ctx, req))
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load()
	if err != nil {
		return Response{Err: err.Error()}
	}

	switch req.Method {
	case MethodSync:
		var res []syncpkg.Result
		err := state.WithLock(func() error {
			r, err := syncpkg.Sync(ctx, st, req.Relpath, req.Origin)
			res = r
			return err
		})
		if err != nil {
			return Response{Err: err.Error()}
		}
		return Response{Results: resultsFromSync(res)}
	case MethodReconcile:
		res, err := reconcile.Reconcile(ctx, st)
		if err != nil {
			return Response{Err: err.Error()}
		}
		return Response{Results: resultsFromReconcile(res)}
	default:
		return Response{Err: fmt.Sprintf("unknown method %q", req.Method)}
	}
}

func writeResponse(conn net.Conn, resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(data, '\n'))
}
