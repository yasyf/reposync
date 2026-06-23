// Package rpc carries sync and reconcile requests from a one-shot client to the
// resident daemon over a unix socket. It is a thin domain wrapper over synckit/rpc:
// the generic {method, params} -> {ok, result, error} transport (framing, dispatch,
// peer-UID check, max-line bound, timeouts) lives in synckit, and this package only
// registers reposync's "sync"/"reconcile" handlers and converts their []Result
// payload to and from the generic result. The cross-host CLI surface
// (`reposync rpc sync --relpath <> --origin <>`) and the SSH command string are the
// frozen interop contract; only the intra-host socket wire is the generic format.
package rpc

import (
	"encoding/json"
	"fmt"

	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/sync"
	synckit "github.com/yasyf/synckit/rpc"
)

// Method names the operation a Request asks the daemon to perform.
type Method string

const (
	// MethodSync asks the daemon to idle-sync the registered repos.
	MethodSync Method = "sync"
	// MethodReconcile asks the daemon to clone-and-sync every registered repo.
	MethodReconcile Method = "reconcile"

	paramRelpath = "relpath"
	paramOrigin  = "origin"
)

// Result reports what the daemon did to one repo.
type Result struct {
	Relpath string `json:"relpath"`
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Err     string `json:"error,omitempty"`
}

// Response is the daemon's reply. Err is set only when the whole request failed;
// per-repo failures live in the matching Results entry.
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

// responseFrom turns a generic synckit Response into reposync's Response, decoding
// the generic result back into []Result on success and carrying the transport error
// otherwise.
func responseFrom(resp *synckit.Response) (*Response, error) {
	if !resp.OK {
		return &Response{Err: resp.Error}, nil
	}
	results, err := resultsFromGeneric(resp.Result)
	if err != nil {
		return nil, err
	}
	return &Response{Results: results}, nil
}

func resultsFromGeneric(result any) ([]Result, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("re-encode rpc result: %w", err)
	}
	var results []Result
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("decode rpc result: %w", err)
	}
	return results, nil
}
