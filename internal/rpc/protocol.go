// Package rpc carries sync and reconcile requests from a one-shot client to the
// resident daemon over a unix socket using a newline-delimited JSON protocol:
// one request line in, one response line out, then the connection closes.
package rpc

import (
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/sync"
)

// Method names the operation a Request asks the daemon to perform.
type Method string

const (
	// MethodSync asks the daemon to idle-sync the registered repos.
	MethodSync Method = "sync"
	// MethodReconcile asks the daemon to clone-and-sync every registered repo.
	MethodReconcile Method = "reconcile"
)

// Request is a single daemon command sent as one JSON line.
type Request struct {
	Method  Method `json:"method"`
	Relpath string `json:"relpath,omitempty"`
	Origin  string `json:"origin,omitempty"`
}

// Result reports what the daemon did to one repo.
type Result struct {
	Relpath string `json:"relpath"`
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Err     string `json:"error,omitempty"`
}

// Response is the daemon's reply sent as one JSON line. Err is set only when the
// whole request failed; per-repo failures live in the matching Results entry.
type Response struct {
	Results []Result `json:"results,omitempty"`
	Err     string   `json:"error,omitempty"`
}

func resultsFromSync(in []sync.Result) []Result {
	out := make([]Result, len(in))
	for i, r := range in {
		out[i] = Result{Relpath: r.Relpath, Outcome: string(r.Outcome), Reason: r.Reason}
		if r.Err != nil {
			out[i].Err = r.Err.Error()
		}
	}
	return out
}

func resultsFromReconcile(in []reconcile.Result) []Result {
	out := make([]Result, len(in))
	for i, r := range in {
		out[i] = Result{Relpath: r.Relpath, Outcome: r.Action}
		if r.Err != nil {
			out[i].Err = r.Err.Error()
		}
	}
	return out
}
