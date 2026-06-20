package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
)

// watchmanConn speaks watchman's newline-delimited JSON protocol over its unix
// socket: each command is one JSON array line, each reply one JSON object line,
// and subscription updates arrive as unsolicited object lines on the same socket.
type watchmanConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialWatchman locates the watchman socket via `watchman get-sockname` and dials it.
func dialWatchman(ctx context.Context) (*watchmanConn, error) {
	sock, err := watchmanSockname(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial watchman socket %s: %w", sock, err)
	}
	return &watchmanConn{conn: conn, r: bufio.NewReader(conn)}, nil
}

func watchmanSockname(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "watchman", "get-sockname").Output()
	if err != nil {
		return "", fmt.Errorf("watchman get-sockname: %w", err)
	}
	var resp struct {
		Sockname string `json:"sockname"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("parse watchman get-sockname: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("watchman get-sockname: %s", resp.Error)
	}
	if resp.Sockname == "" {
		return "", fmt.Errorf("watchman get-sockname returned an empty socket path")
	}
	return resp.Sockname, nil
}

func (c *watchmanConn) close() error { return c.conn.Close() }

func (c *watchmanConn) send(cmd ...any) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("encode watchman command: %w", err)
	}
	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write watchman command: %w", err)
	}
	return nil
}

// readPDU reads one newline-delimited JSON object. ReadBytes (not bufio.Scanner)
// avoids a token-size cap, since subscription PDUs can be large.
func (c *watchmanConn) readPDU() (map[string]json.RawMessage, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var pdu map[string]json.RawMessage
	if err := json.Unmarshal(line, &pdu); err != nil {
		return nil, fmt.Errorf("decode watchman pdu: %w", err)
	}
	return pdu, nil
}

// request sends one command and returns its reply, handing any subscription PDUs
// that interleave before the reply to onSubscription. A non-empty "error" field
// in the reply becomes a Go error.
func (c *watchmanConn) request(onSubscription func(map[string]json.RawMessage), cmd ...any) (map[string]json.RawMessage, error) {
	if err := c.send(cmd...); err != nil {
		return nil, err
	}
	for {
		pdu, err := c.readPDU()
		if err != nil {
			return nil, err
		}
		if _, ok := pdu["subscription"]; ok {
			if onSubscription != nil {
				onSubscription(pdu)
			}
			continue
		}
		if raw, ok := pdu["error"]; ok {
			var msg string
			_ = json.Unmarshal(raw, &msg)
			return nil, fmt.Errorf("watchman error: %s", msg)
		}
		return pdu, nil
	}
}

// subscribe watches dir as its own root and subscribes to changes under it,
// scoping to changes after the current clock so the daemon does not act on the
// repo's existing state at startup. dir must be a VCS metadata leaf directory
// (e.g. .git/refs/remotes/origin), never the repo root (which watchman ignores as
// a VCS dir) nor a recursive parent like .git (which would crawl the object store).
func (c *watchmanConn) subscribe(dir, name string, onSubscription func(map[string]json.RawMessage)) error {
	watchResp, err := c.request(onSubscription, "watch", dir)
	if err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	root := dir
	if raw, ok := watchResp["watch"]; ok {
		_ = json.Unmarshal(raw, &root)
	}

	clockResp, err := c.request(onSubscription, "clock", root)
	if err != nil {
		return fmt.Errorf("clock %s: %w", root, err)
	}
	var clock string
	if raw, ok := clockResp["clock"]; ok {
		_ = json.Unmarshal(raw, &clock)
	}

	query := map[string]any{
		"since":                   clock,
		"fields":                  []string{"name"},
		"empty_on_fresh_instance": true,
	}
	if _, err := c.request(onSubscription, "subscribe", root, name, query); err != nil {
		return fmt.Errorf("subscribe %s: %w", root, err)
	}
	return nil
}

// runSubscriptions reads subscription PDUs until ctx is canceled (which closes the
// connection to unblock the read), dispatching each to onSubscription.
func (c *watchmanConn) runSubscriptions(ctx context.Context, onSubscription func(map[string]json.RawMessage)) error {
	go func() {
		<-ctx.Done()
		_ = c.conn.Close()
	}()
	for {
		pdu, err := c.readPDU()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read watchman pdu: %w", err)
		}
		if _, ok := pdu["subscription"]; ok {
			onSubscription(pdu)
		}
	}
}
