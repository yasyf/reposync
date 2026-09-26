package worktree

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/yasyf/reposync/internal/vcs"
)

var statusArgs = []string{
	"status", "--porcelain=v2", "-z", "--branch",
	"--untracked-files=all", "--ignored=no", "--no-renames", "--ignore-submodules=none",
}

type statusReport struct {
	commit    string
	branch    string
	upstream  string
	changed   []statusEntry
	unmerged  []string
	untracked []string
}

type statusEntry struct {
	x, y         byte
	sub          string
	modeHead     string
	modeIndex    string
	modeWorktree string
	oidHead      string
	oidIndex     string
	path         string
}

func (e statusEntry) submoduleChanged() bool {
	return e.sub != "N..." && strings.ContainsAny(e.sub[1:], "CMU")
}

func readStatus(ctx context.Context, dir string, env []string, pathspec ...string) (statusReport, error) {
	args := append([]string{"-C", dir}, statusArgs...)
	if len(pathspec) > 0 {
		args = append(append(args, "--"), pathspec...)
	}
	var out bytes.Buffer
	if err := vcs.Exec(ctx, vcs.Cmd{Dir: dir, Name: "git", Args: args, Env: env, Stdout: &out}); err != nil {
		return statusReport{}, fmt.Errorf("status %s: %w", dir, err)
	}
	return parseStatus(out.Bytes())
}

func parseStatus(out []byte) (statusReport, error) {
	var r statusReport
	if len(out) > 0 && out[len(out)-1] != 0 {
		return r, fmt.Errorf("status output not NUL-terminated")
	}
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		if err := r.add(rec); err != nil {
			return statusReport{}, err
		}
	}
	return r, nil
}

func (r *statusReport) add(rec string) error {
	switch rec[0] {
	case '#':
		key, value, _ := strings.Cut(strings.TrimPrefix(rec, "# "), " ")
		switch key {
		case "branch.oid":
			if value != "(initial)" {
				r.commit = value
			}
		case "branch.head":
			if value != "(detached)" {
				r.branch = value
			}
		case "branch.upstream":
			r.upstream = value
		}
		return nil
	case '1':
		f := strings.SplitN(rec, " ", 9)
		if len(f) != 9 || len(f[1]) != 2 || len(f[2]) != 4 {
			return fmt.Errorf("malformed status record %q", rec)
		}
		r.changed = append(r.changed, statusEntry{
			x: f[1][0], y: f[1][1], sub: f[2],
			modeHead: f[3], modeIndex: f[4], modeWorktree: f[5],
			oidHead: f[6], oidIndex: f[7], path: f[8],
		})
		return nil
	case 'u':
		f := strings.SplitN(rec, " ", 11)
		if len(f) != 11 {
			return fmt.Errorf("malformed status record %q", rec)
		}
		r.unmerged = append(r.unmerged, f[10])
		return nil
	case '?':
		path, ok := strings.CutPrefix(rec, "? ")
		if !ok || path == "" {
			return fmt.Errorf("malformed status record %q", rec)
		}
		r.untracked = append(r.untracked, path)
		return nil
	}
	return fmt.Errorf("unexpected status record %q", rec)
}
