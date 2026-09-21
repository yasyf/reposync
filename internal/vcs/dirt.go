package vcs

import (
	"context"
	"strings"
)

const untrackedStatus = "??"

// dirtyPath is one dirty path and the porcelain status code of its record.
type dirtyPath struct {
	path   string
	status string
}

// dirt is a git working tree's uncommitted changes, classified: generated is
// the dirty paths marked linguist-generated, untracked is every untracked path
// (generated or not), blocking is the dirty paths that are tracked and not
// generated. An untracked path is never blocking — a fast-forward leaves it
// where it is, or declines rather than clobber it.
type dirt struct {
	generated []string
	untracked []string
	blocking  []string
}

func dirtState(ctx context.Context, path string) (dirt, error) {
	status, err := run(ctx, path, "git", "-C", path, "status", "--porcelain", "-uall", "-z")
	if err != nil {
		return dirt{}, err
	}
	dirty := parsePorcelainZ(status)
	if len(dirty) == 0 {
		return dirt{}, nil
	}
	paths := make([]string, 0, len(dirty))
	for _, d := range dirty {
		paths = append(paths, d.path)
	}
	generated, err := generatedPaths(ctx, path, paths)
	if err != nil {
		return dirt{}, err
	}
	isGenerated := pathSet(generated)
	state := dirt{generated: generated}
	for _, d := range dirty {
		if d.status == untrackedStatus {
			state.untracked = append(state.untracked, d.path)
			continue
		}
		if _, ok := isGenerated[d.path]; ok {
			continue
		}
		state.blocking = append(state.blocking, d.path)
	}
	return state, nil
}

// parsePorcelainZ extracts the dirty paths and status codes of `git status
// --porcelain -z` output. A rename or copy emits the old path as a bare
// following record; both endpoints are returned under the record's status, so
// a rename of real work into a generated-named path stays blocking dirt.
func parsePorcelainZ(out string) []dirtyPath {
	records := strings.Split(out, "\x00")
	var dirty []dirtyPath
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		status := rec[:2]
		dirty = append(dirty, dirtyPath{path: rec[3:], status: status})
		if status[0] == 'R' || status[0] == 'C' {
			i++
			if i < len(records) && records[i] != "" {
				dirty = append(dirty, dirtyPath{path: records[i], status: status})
			}
		}
	}
	return dirty
}

// generatedPaths returns the subset of paths whose linguist-generated attribute
// resolves to a set value, via `git check-attr -z --stdin`.
func generatedPaths(ctx context.Context, path string, paths []string) ([]string, error) {
	stdin := strings.Join(paths, "\x00") + "\x00"
	out, err := runStdin(ctx, path, stdin, "git", "-C", path, "check-attr", "-z", "linguist-generated", "--stdin")
	if err != nil {
		return nil, err
	}
	return parseCheckAttrZ(out), nil
}

// parseCheckAttrZ parses `git check-attr -z` output into the list of paths whose
// value is set ("set" or "true"); other values mean not generated.
func parseCheckAttrZ(out string) []string {
	fields := strings.Split(out, "\x00")
	var gen []string
	for i := 0; i+2 < len(fields); i += 3 {
		p, value := fields[i], fields[i+2]
		if value == "set" || value == "true" {
			gen = append(gen, p)
		}
	}
	return gen
}
