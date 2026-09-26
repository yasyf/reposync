package worktree

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	lfsSpecLine   = "version https://git-lfs.github.com/spec/v1"
	lfsPointerMax = 1024
	lfsFilter     = "lfs"
)

var (
	lfsVersions = []string{"https://git-lfs.github.com/spec/v1", "https://hawser.github.com/spec/v1", "http://git-media.io/v/2"}
	lfsExtKey   = regexp.MustCompile(`^ext-[0-9]`)
)

type lfsPointer struct {
	OID       string
	Size      int64
	Extra     []string
	Canonical bool
}

func parseLFSPointer(b []byte) (lfsPointer, bool) {
	if len(b) > lfsPointerMax {
		return lfsPointer{}, false
	}
	var p lfsPointer
	keys := []string{"version", "oid", "size"}
	for line := range strings.SplitSeq(string(bytes.TrimSpace(b)), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok || len(keys) == 0 {
			return lfsPointer{}, false
		}
		if key != keys[0] {
			if !lfsExtKey.MatchString(key) {
				return lfsPointer{}, false
			}
			p.Extra = append(p.Extra, line)
			continue
		}
		keys = keys[1:]
		switch key {
		case "version":
			if !slices.Contains(lfsVersions, value) {
				return lfsPointer{}, false
			}
		case "oid":
			hexOID, ok := strings.CutPrefix(value, "sha256:")
			if !ok || !isHex(hexOID, 64) {
				return lfsPointer{}, false
			}
			p.OID = hexOID
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return lfsPointer{}, false
			}
			p.Size = n
		}
	}
	if len(keys) > 0 {
		return lfsPointer{}, false
	}
	p.Canonical = bytes.Equal(p.encode(), b)
	return p, true
}

func (p lfsPointer) encode() []byte {
	var b strings.Builder
	b.WriteString(lfsSpecLine + "\n")
	for _, e := range p.Extra {
		b.WriteString(e + "\n")
	}
	fmt.Fprintf(&b, "oid sha256:%s\nsize %d\n", p.OID, p.Size)
	return []byte(b.String())
}

func (p lfsPointer) artifact() ArtifactRef {
	return ArtifactRef{Digest: digestPrefix + p.OID, Size: p.Size, Media: MediaLFSObject}
}

func (s source) readPointers(ctx context.Context, oids []string) (map[string]lfsPointer, error) {
	sizes, err := s.blobSizes(ctx, oids)
	if err != nil {
		return nil, err
	}
	var small []string
	for _, oid := range oids {
		if sizes[oid] <= lfsPointerMax {
			small = append(small, oid)
		}
	}
	pointers := map[string]lfsPointer{}
	err = s.readBlobs(ctx, small, func(oid string, _ int64, r io.Reader) error {
		b, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("read blob %s: %w", oid, err)
		}
		if p, ok := parseLFSPointer(b); ok {
			pointers[oid] = p
		}
		return nil
	})
	return pointers, err
}

type stagedBlob struct {
	path, oid    string
	skipWorktree bool
}

func stagedBlobs(listing string) []stagedBlob {
	var blobs []stagedBlob
	for rec := range strings.SplitSeq(strings.TrimSuffix(listing, "\x00"), "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || (f[1] != "100644" && f[1] != "100755") {
			continue
		}
		blobs = append(blobs, stagedBlob{path: p, oid: f[2], skipWorktree: f[0] == "S"})
	}
	return blobs
}

type historyBlob struct {
	commit, path, oid string
}

func (s source) historyLFS(ctx context.Context, head, trunkTip string) ([]LFSObjectRef, error) {
	var commits, out bytes.Buffer
	if err := s.run(ctx, nil, &commits, "rev-list", head, "^"+trunkTip); err != nil {
		return nil, err
	}
	transitions, err := s.attributeHistoryLFS(ctx, commits.String())
	if err != nil {
		return nil, err
	}
	if err := s.run(ctx, &commits, &out, "diff-tree", "--stdin", "-r", "-m", "--root", "--raw", "--no-abbrev", "-z", "--no-renames"); err != nil {
		return nil, err
	}
	blobs, err := parseRawDiff(out.String(), head)
	if err != nil {
		return nil, err
	}
	oids := make([]string, 0, len(blobs))
	for _, b := range blobs {
		oids = append(oids, b.oid)
	}
	slices.Sort(oids)
	pointers, err := s.readPointers(ctx, slices.Compact(oids))
	if err != nil {
		return nil, err
	}
	ambiguous := map[string][]string{}
	for _, b := range blobs {
		if p, ok := pointers[b.oid]; ok && !p.Canonical {
			ambiguous[b.commit] = append(ambiguous[b.commit], b.path)
		}
	}
	attrs := map[string]map[string]string{}
	for _, commit := range slices.Sorted(maps.Keys(ambiguous)) {
		paths := ambiguous[commit]
		slices.Sort(paths)
		if attrs[commit], err = s.filterAttr(ctx, slices.Compact(paths), "--source="+commit); err != nil {
			return nil, err
		}
	}
	refs := transitions
	for _, b := range blobs {
		if p, ok := pointers[b.oid]; ok && (p.Canonical || attrs[b.commit][b.path] == lfsFilter) {
			refs = append(refs, LFSObjectRef{Path: b.path, OID: p.OID, Size: p.Size})
		}
	}
	return refs, nil
}

func (s source) attributeHistoryLFS(ctx context.Context, commits string) ([]LFSObjectRef, error) {
	var out bytes.Buffer
	if err := s.run(ctx, strings.NewReader(commits), &out, "diff-tree", "--stdin", "-r", "-m", "--root", "--name-only", "-z", "--no-renames", "--", ":(glob)**/.gitattributes"); err != nil {
		return nil, err
	}
	dirs := map[string][]string{}
	var commit string
	for tok := range strings.SplitSeq(out.String(), "\x00") {
		switch tok = strings.Trim(tok, "\n"); {
		case tok == "":
		case isHex(tok, 40) || isHex(tok, 64):
			commit = tok
		default:
			dirs[commit] = append(dirs[commit], path.Dir(tok))
		}
	}
	var refs []LFSObjectRef
	for _, commit := range slices.Sorted(maps.Keys(dirs)) {
		slices.Sort(dirs[commit])
		got, err := s.attributeTransitions(ctx, commit, slices.Compact(dirs[commit]))
		if err != nil {
			return nil, err
		}
		refs = append(refs, got...)
	}
	return refs, nil
}

func (s source) attributeTransitions(ctx context.Context, commit string, dirs []string) ([]LFSObjectRef, error) {
	ids, err := s.output(ctx, "rev-list", "--parents", "-n1", commit)
	if err != nil {
		return nil, err
	}
	var listed bytes.Buffer
	if err := s.run(ctx, nil, &listed, append([]string{"ls-tree", "-r", "-z", commit, "--"}, dirs...)...); err != nil {
		return nil, err
	}
	oids := map[string]string{}
	for rec := range strings.SplitSeq(strings.TrimSuffix(listed.String(), "\x00"), "\x00") {
		meta, path, _ := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); len(f) == 3 && (f[0] == "100644" || f[0] == "100755") {
			oids[path] = f[2]
		}
	}
	paths := slices.Sorted(maps.Keys(oids))
	after, err := s.filterAttr(ctx, paths, "--source="+commit)
	if err != nil {
		return nil, err
	}
	var before []map[string]string
	for _, parent := range strings.Fields(ids)[1:] {
		attrs, err := s.filterAttr(ctx, paths, "--source="+parent)
		if err != nil {
			return nil, err
		}
		before = append(before, attrs)
	}
	var transitioned, blobs []string
	for _, p := range paths {
		if after[p] == lfsFilter && (len(before) == 0 || slices.ContainsFunc(before, func(attrs map[string]string) bool { return attrs[p] != lfsFilter })) {
			transitioned = append(transitioned, p)
			blobs = append(blobs, oids[p])
		}
	}
	slices.Sort(blobs)
	pointers, err := s.readPointers(ctx, slices.Compact(blobs))
	if err != nil {
		return nil, err
	}
	var refs []LFSObjectRef
	for _, p := range transitioned {
		if ptr, ok := pointers[oids[p]]; ok {
			refs = append(refs, LFSObjectRef{Path: p, OID: ptr.OID, Size: ptr.Size})
		}
	}
	return refs, nil
}

func parseRawDiff(out, commit string) ([]historyBlob, error) {
	var blobs []historyBlob
	tokens := strings.Split(out, "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := strings.Trim(tokens[i], "\n")
		if tok == "" {
			continue
		}
		if isHex(tok, 40) || isHex(tok, 64) {
			commit = tok
			continue
		}
		if !strings.HasPrefix(tok, ":") || i+1 >= len(tokens) {
			return nil, fmt.Errorf("raw diff record %q", tok)
		}
		i++
		f := strings.Fields(tok)
		if len(f) != 5 {
			return nil, fmt.Errorf("raw diff record %q", tok)
		}
		mode, oid := f[1], f[3]
		if (mode == "100644" || mode == "100755") && strings.Trim(oid, "0") != "" {
			blobs = append(blobs, historyBlob{commit: commit, path: tokens[i], oid: oid})
		}
	}
	return blobs, nil
}
