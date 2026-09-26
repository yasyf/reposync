package worktree

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type history struct {
	links    []Bundle
	requires []string
}

func (s *Store) captureHistory(ctx context.Context, wt Worktree, objectFormat string, head Head, l *ledger, sink ArtifactSink, maxLinks int, b *budget) (history, error) {
	if head.Ahead == 0 {
		l.Chain = nil
		return history{requires: []string{head.Commit}}, nil
	}
	scratch := s.scratchDir(wt.Origin)
	if err := initBareAlternate(ctx, scratch, objectFormat, filepath.Join(wt.CommonDir, "objects")); err != nil {
		return history{}, err
	}
	if n := len(l.Chain); n > 0 {
		published, err := allPublished(ctx, scratch, l.Chain[n-1].Requires, head.TrunkTip)
		if err != nil {
			return history{}, err
		}
		if !published {
			l.Chain = nil
		} else {
			held, err := chainHeld(ctx, l.Chain, sink)
			if err != nil {
				return history{}, err
			}
			if !held {
				l.Chain = nil
			}
		}
	}
	if n := len(l.Chain); n > 0 && l.Chain[n-1].Bundle.Tip == head.Commit {
		return chainHistory(l.Chain), nil
	}
	excludes := []string{head.TrunkTip}
	appending, err := canAppend(ctx, scratch, l.Chain, head.Commit, maxLinks)
	if err != nil {
		return history{}, err
	}
	if appending {
		excludes = append(excludes, l.Chain[len(l.Chain)-1].Bundle.Tip)
	} else {
		l.Chain = nil
	}
	slices.Sort(excludes)
	excludes = slices.Compact(excludes)
	link, err := s.cutBundle(ctx, scratch, wt.ID, head.Commit, excludes, sink)
	if err != nil {
		return history{}, err
	}
	b.spend(link.Bundle.Artifact.Size)
	if link.Requires, err = chainRequires(ctx, scratch, l.Chain, link.Bundle.Prerequisites); err != nil {
		return history{}, err
	}
	l.Chain = append(l.Chain, link)
	return chainHistory(l.Chain), nil
}

func allPublished(ctx context.Context, scratch string, commits []string, trunkTip string) (bool, error) {
	for _, c := range commits {
		present, err := scratchTest(ctx, scratch, "cat-file", "-e", c)
		if err != nil || !present {
			return false, err
		}
		published, err := scratchTest(ctx, scratch, "merge-base", "--is-ancestor", c, trunkTip)
		if err != nil || !published {
			return false, err
		}
	}
	return true, nil
}

func chainHeld(ctx context.Context, chain []chainLink, sink ArtifactSink) (bool, error) {
	refs := make([]ArtifactRef, len(chain))
	for i, c := range chain {
		refs[i] = c.Bundle.Artifact
	}
	has, err := sink.Has(ctx, refs)
	if err != nil {
		return false, fmt.Errorf("sink has: %w", err)
	}
	return !slices.Contains(has, false), nil
}

func chainHistory(chain []chainLink) history {
	h := history{requires: chain[len(chain)-1].Requires}
	for _, c := range chain {
		h.links = append(h.links, c.Bundle)
	}
	return h
}

func canAppend(ctx context.Context, scratch string, chain []chainLink, head string, maxLinks int) (bool, error) {
	if len(chain) == 0 || len(chain) >= maxLinks {
		return false, nil
	}
	tail := chain[len(chain)-1].Bundle.Tip
	present, err := scratchTest(ctx, scratch, "cat-file", "-e", tail)
	if err != nil || !present {
		return false, err
	}
	return scratchTest(ctx, scratch, "merge-base", "--is-ancestor", tail, head)
}

func scratchTest(ctx context.Context, scratch string, args ...string) (bool, error) {
	err := storeGit(ctx, scratch, nil, args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) cutBundle(ctx context.Context, scratch, worktreeID, head string, excludes []string, sink ArtifactSink) (chainLink, error) {
	ref := "refs/capture/" + worktreeID + "/tip"
	if err := storeGit(ctx, scratch, nil, "update-ref", ref, head); err != nil {
		return chainLink{}, err
	}
	tmp, err := os.CreateTemp(s.tempDir(), "bundle-*")
	if err != nil {
		return chainLink{}, fmt.Errorf("create bundle temp: %w", err)
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	if err := tmp.Close(); err != nil {
		return chainLink{}, fmt.Errorf("close bundle temp: %w", err)
	}
	args := []string{"bundle", "create", "-q", path, ref}
	for _, e := range excludes {
		args = append(args, "^"+e)
	}
	if err := storeGit(ctx, scratch, nil, args...); err != nil {
		return chainLink{}, err
	}
	prereqs, err := bundlePrerequisites(path)
	if err != nil {
		return chainLink{}, err
	}
	var heads bytes.Buffer
	if err := storeGit(ctx, scratch, &heads, "bundle", "list-heads", path); err != nil {
		return chainLink{}, err
	}
	if got := strings.TrimSpace(heads.String()); got != head+" "+ref {
		return chainLink{}, fmt.Errorf("bundle heads %q, want %s %s", got, head, ref)
	}
	//nolint:gosec // G304: a bundle this store just cut into its own temp dir.
	f, err := os.Open(path)
	if err != nil {
		return chainLink{}, fmt.Errorf("open bundle: %w", err)
	}
	defer func() { _ = f.Close() }()
	artifact, err := sink.Put(ctx, MediaBundle, f)
	if err != nil {
		return chainLink{}, fmt.Errorf("put bundle: %w", err)
	}
	return chainLink{
		Bundle:   Bundle{Artifact: artifact, Tip: head, Prerequisites: prereqs},
		Excludes: excludes,
	}, nil
}

func bundlePrerequisites(path string) ([]string, error) {
	//nolint:gosec // G304: a bundle this store just cut into its own temp dir.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	signature, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read bundle signature: %w", err)
	}
	if signature != "# v2 git bundle\n" && signature != "# v3 git bundle\n" {
		return nil, fmt.Errorf("bundle signature %q", signature)
	}
	var prereqs []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read bundle header: %w", err)
		}
		if line == "\n" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "-"); ok {
			oid, _, _ := strings.Cut(strings.TrimSuffix(rest, "\n"), " ")
			prereqs = append(prereqs, oid)
		}
	}
	slices.Sort(prereqs)
	return slices.Compact(prereqs), nil
}

func chainRequires(ctx context.Context, scratch string, chain []chainLink, prereqs []string) ([]string, error) {
	if len(chain) == 0 {
		return prereqs, nil
	}
	requires := slices.Clone(chain[len(chain)-1].Requires)
	var produced map[string]bool
	for _, p := range prereqs {
		if slices.ContainsFunc(chain, func(c chainLink) bool { return c.Bundle.Tip == p }) {
			continue
		}
		if produced == nil {
			var err error
			if produced, err = chainCommits(ctx, scratch, chain); err != nil {
				return nil, err
			}
		}
		if !produced[p] {
			requires = append(requires, p)
		}
	}
	slices.Sort(requires)
	return slices.Compact(requires), nil
}

func chainCommits(ctx context.Context, scratch string, chain []chainLink) (map[string]bool, error) {
	commits := map[string]bool{}
	for _, c := range chain {
		args := make([]string, 0, 2+len(c.Excludes))
		args = append(args, "rev-list", c.Bundle.Tip)
		for _, e := range c.Excludes {
			args = append(args, "^"+e)
		}
		var out bytes.Buffer
		if err := storeGit(ctx, scratch, &out, args...); err != nil {
			return nil, err
		}
		for oid := range strings.FieldsSeq(out.String()) {
			commits[oid] = true
		}
	}
	return commits, nil
}
