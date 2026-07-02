package vcs

import (
	"context"
	"strings"
)

// dirtState classifies a git working tree's uncommitted changes. clean is true
// when nothing is dirty; generatedOnly is true when the tree is dirty and every
// dirty path is marked linguist-generated; generated is the list of dirty paths
// that are generated.
func dirtState(ctx context.Context, path string) (clean, generatedOnly bool, generated []string, err error) {
	status, err := run(ctx, path, "git", "-C", path, "status", "--porcelain", "-uall", "-z")
	if err != nil {
		return false, false, nil, err
	}
	dirty := parsePorcelainZ(status)
	if len(dirty) == 0 {
		return true, false, nil, nil
	}
	gen, err := generatedPaths(ctx, path, dirty)
	if err != nil {
		return false, false, nil, err
	}
	return false, len(gen) == len(dirty), gen, nil
}

// parsePorcelainZ extracts the dirty paths from each record of `git status
// --porcelain -z` output. Rename and copy records emit the new path in the
// status record and the old path as a bare following record; both endpoints are
// returned so a rename of a non-generated file into a generated-named path is
// still classified by its source and never mistaken for generated-only dirt.
func parsePorcelainZ(out string) []string {
	records := strings.Split(out, "\x00")
	var paths []string
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		x := rec[0]
		paths = append(paths, rec[3:])
		if x == 'R' || x == 'C' {
			i++
			if i < len(records) && records[i] != "" {
				paths = append(paths, records[i])
			}
		}
	}
	return paths
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
