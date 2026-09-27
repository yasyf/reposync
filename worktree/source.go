package worktree

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	got, err := s.checkAttr(ctx, paths, []string{"filter"}, opts...)
	if err != nil {
		return nil, err
	}
	attrs := map[string]string{}
	for p, v := range got {
		attrs[p] = v["filter"]
	}
	return attrs, nil
}

func (s source) checkAttr(ctx context.Context, paths, names []string, opts ...string) (map[string]map[string]string, error) {
	attrs := map[string]map[string]string{}
	if len(paths) == 0 {
		return attrs, nil
	}
	var out bytes.Buffer
	stdin := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	args := append(append(append([]string{"check-attr"}, opts...), "-z", "--stdin"), names...)
	if err := s.run(ctx, stdin, &out, args...); err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("check-attr output has %d fields", len(fields))
	}
	for i := 0; i < len(fields); i += 3 {
		if attrs[fields[i]] == nil {
			attrs[fields[i]] = map[string]string{}
		}
		attrs[fields[i]][fields[i+1]] = fields[i+2]
	}
	return attrs, nil
}

func (s source) intentToAdd(ctx context.Context) (map[string]string, error) {
	cached := func(visibility string) (map[string]string, error) {
		var out bytes.Buffer
		if err := s.run(ctx, nil, &out, "diff-index", "--cached", "--raw", "--no-abbrev", "--no-renames", "-z", visibility, "HEAD"); err != nil {
			return nil, err
		}
		records := map[string]string{}
		if out.Len() == 0 {
			return records, nil
		}
		tokens := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
		if len(tokens)%2 != 0 {
			return nil, fmt.Errorf("diff-index output has %d fields", len(tokens))
		}
		for i := 0; i < len(tokens); i += 2 {
			if f := strings.Fields(tokens[i]); len(f) != 5 || !strings.HasPrefix(f[0], ":") {
				return nil, fmt.Errorf("diff-index record %q", tokens[i])
			}
			records[tokens[i+1]] = tokens[i]
		}
		return records, nil
	}
	visible, err := cached("--ita-visible-in-index")
	if err != nil {
		return nil, err
	}
	invisible, err := cached("--ita-invisible-in-index")
	if err != nil {
		return nil, err
	}
	ita := map[string]string{}
	for p, rec := range visible {
		if invisible[p] != rec {
			ita[p] = strings.Fields(rec)[1]
		}
	}
	for p, rec := range invisible {
		if _, ok := visible[p]; !ok {
			ita[p] = strings.TrimPrefix(strings.Fields(rec)[0], ":")
		}
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

func (s source) checkoutDigests(ctx context.Context, attrSource string, blobs []stagedBlob) (map[string]ArtifactRef, error) {
	refs := map[string]ArtifactRef{}
	if len(blobs) == 0 {
		return refs, nil
	}
	scratch, err := os.MkdirTemp("", "reposync-checkout-")
	if err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	var paths strings.Builder
	for _, b := range blobs {
		paths.WriteString(b.path + "\x00")
	}
	args := []string{"checkout-index", "-z", "--stdin", "--prefix=" + scratch + string(filepath.Separator)}
	if attrSource != "" {
		args = append([]string{"--attr-source=" + attrSource}, args...)
	}
	if err := s.run(ctx, strings.NewReader(paths.String()), nil, args...); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return nil, fmt.Errorf("open scratch dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	for _, b := range blobs {
		ref, err := readOnce(root, filepath.FromSlash(b.path), func(r io.Reader) (ArtifactRef, error) {
			h := sha256.New()
			n, err := io.Copy(h, r)
			return ArtifactRef{Digest: digestPrefix + hex.EncodeToString(h.Sum(nil)), Size: n, Media: MediaFile}, err
		})
		if err != nil {
			return nil, fmt.Errorf("checked-out %s: %w", b.path, err)
		}
		refs[b.path] = ref
	}
	return refs, nil
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
