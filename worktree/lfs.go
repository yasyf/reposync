package worktree

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const (
	lfsSpecLine   = "version https://git-lfs.github.com/spec/v1"
	lfsPointerMax = 1024
	lfsFilter     = "lfs"
)

type lfsPointer struct {
	OID       string   `json:"oid"`
	Size      int64    `json:"size"`
	Extra     []string `json:"extra,omitempty"`
	Canonical bool     `json:"canonical"`
}

func parseLFSPointer(b []byte) (lfsPointer, bool) {
	if len(b) > lfsPointerMax || !bytes.HasSuffix(b, []byte("\n")) {
		return lfsPointer{}, false
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if lines[0] != lfsSpecLine {
		return lfsPointer{}, false
	}
	var p lfsPointer
	var haveOID, haveSize bool
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return lfsPointer{}, false
		}
		switch key {
		case "oid":
			hexOID, ok := strings.CutPrefix(value, "sha256:")
			if !ok || !isHex(hexOID, 64) {
				return lfsPointer{}, false
			}
			p.OID, haveOID = hexOID, true
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return lfsPointer{}, false
			}
			p.Size, haveSize = n, true
		default:
			p.Extra = append(p.Extra, line)
		}
	}
	p.Canonical = bytes.Equal(p.encode(), b)
	return p, haveOID && haveSize
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

func (p lfsPointer) strict() bool {
	return p.Canonical && !slices.ContainsFunc(p.Extra, func(e string) bool { return !strings.HasPrefix(e, "ext-") })
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

type historyBlob struct {
	commit, path, oid string
}

func (s source) historyLFS(ctx context.Context, head, trunkTip, trunkBase string) ([]LFSObjectRef, error) {
	var blobs []historyBlob
	for _, args := range [][]string{
		{"log", "--format=%H", "--raw", "--no-abbrev", "-z", "--no-renames", head, "^" + trunkTip},
		{"diff-tree", "-r", "--raw", "--no-abbrev", "-z", "--no-renames", trunkBase, head},
	} {
		var out bytes.Buffer
		if err := s.run(ctx, nil, &out, args...); err != nil {
			return nil, err
		}
		parsed, err := parseRawDiff(out.String(), head)
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, parsed...)
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
		if p, ok := pointers[b.oid]; ok && !p.strict() {
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
	var refs []LFSObjectRef
	for _, b := range blobs {
		if p, ok := pointers[b.oid]; ok && (p.strict() || attrs[b.commit][b.path] == lfsFilter) {
			refs = append(refs, LFSObjectRef{Path: b.path, OID: p.OID, Size: p.Size})
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
