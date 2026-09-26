package worktree

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/yasyf/reposync/internal/vcs"
)

type source struct {
	dir string
	env []string
}

func newSource(ctx context.Context, wt Worktree, privateIndex, parent string) (source, error) {
	top, list := wt.Root, []string{"ls-files", "-z", "--stage"}
	var repo []string
	if wt.Kind == KindJJWorkspace {
		top, list = wt.CommonDir, []string{"ls-tree", "-r", "-z", parent}
		repo = []string{"GIT_DIR=" + wt.CommonDir, "GIT_WORK_TREE=" + wt.Root, "GIT_INDEX_FILE=" + privateIndex}
	}
	subs, err := gitlinkRoots(ctx, wt.Root, append(vcs.ReadOnlyGitEnv(), repo...), list...)
	if err != nil {
		return source{}, err
	}
	env, err := vcs.FilterOverrideEnv(ctx, append([]string{top}, subs...)...)
	if err != nil {
		return source{}, err
	}
	return source{dir: wt.Root, env: append(env, repo...)}, nil
}

func (s source) run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	err := vcs.Exec(ctx, vcs.Cmd{Dir: s.dir, Name: "git", Args: append([]string{"-C", s.dir}, args...), Env: s.env, Stdin: stdin, Stdout: stdout})
	if err != nil {
		return fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), s.dir, err)
	}
	return nil
}

func (s source) output(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	if err := s.run(ctx, nil, &out, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func (s source) config(ctx context.Context, pattern string, opts ...string) (map[string]string, error) {
	args := append([]string{"config", "-z"}, opts...)
	var out bytes.Buffer
	err := s.run(ctx, nil, &out, append(args, "--get-regexp", pattern)...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for rec := range strings.SplitSeq(strings.TrimSuffix(out.String(), "\x00"), "\x00") {
		key, value, _ := strings.Cut(rec, "\n")
		values[key] = value
	}
	return values, nil
}

func (s source) filterAttr(ctx context.Context, paths []string, opts ...string) (map[string]string, error) {
	attrs := map[string]string{}
	if len(paths) == 0 {
		return attrs, nil
	}
	var out bytes.Buffer
	stdin := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	args := append(append([]string{"check-attr"}, opts...), "-z", "--stdin", "filter")
	if err := s.run(ctx, stdin, &out, args...); err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("check-attr output has %d fields", len(fields))
	}
	for i := 0; i < len(fields); i += 3 {
		attrs[fields[i]] = fields[i+2]
	}
	return attrs, nil
}

func (s source) intentToAdd(ctx context.Context) (map[string]bool, error) {
	added := func(visibility string) (map[string]bool, error) {
		var out bytes.Buffer
		if err := s.run(ctx, nil, &out, "diff-index", "--cached", "--name-only", "--diff-filter=A", "-z", visibility, "HEAD"); err != nil {
			return nil, err
		}
		paths := map[string]bool{}
		for p := range strings.SplitSeq(strings.TrimSuffix(out.String(), "\x00"), "\x00") {
			if p != "" {
				paths[p] = true
			}
		}
		return paths, nil
	}
	ita, err := added("--ita-visible-in-index")
	if err != nil {
		return nil, err
	}
	staged, err := added("--ita-invisible-in-index")
	if err != nil {
		return nil, err
	}
	for p := range staged {
		delete(ita, p)
	}
	return ita, nil
}

func (s source) blobSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	sizes := map[string]int64{}
	if len(oids) == 0 {
		return sizes, nil
	}
	var out bytes.Buffer
	stdin := strings.NewReader(strings.Join(oids, "\n") + "\n")
	if err := s.run(ctx, stdin, &out, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)"); err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(out.String(), "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			return nil, fmt.Errorf("cat-file: %q is not a blob", line)
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cat-file size %q: %w", line, err)
		}
		sizes[f[0]] = size
	}
	return sizes, nil
}

func (s source) readBlobs(ctx context.Context, oids []string, each func(oid string, size int64, r io.Reader) error) error {
	if len(oids) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.run(ctx, strings.NewReader(strings.Join(oids, "\n")+"\n"), pw, "cat-file", "--batch")
		_ = pw.CloseWithError(err)
		done <- err
	}()
	err := readBatch(bufio.NewReader(pr), oids, each)
	if err != nil {
		cancel()
		_ = pr.CloseWithError(err)
		<-done
		return err
	}
	return <-done
}

func readBatch(br *bufio.Reader, oids []string, each func(oid string, size int64, r io.Reader) error) error {
	for _, oid := range oids {
		header, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("cat-file header for %s: %w", oid, err)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != oid || f[1] != "blob" {
			return fmt.Errorf("cat-file %s: header %q", oid, strings.TrimSpace(header))
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return fmt.Errorf("cat-file %s size: %w", oid, err)
		}
		body := io.LimitReader(br, size)
		if err := each(oid, size, body); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return fmt.Errorf("cat-file %s: %w", oid, err)
		}
		if b, err := br.ReadByte(); err != nil || b != '\n' {
			return fmt.Errorf("cat-file %s: missing record terminator", oid)
		}
	}
	return nil
}
