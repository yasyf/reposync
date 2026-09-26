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

func (s source) historyLFS(ctx context.Context, head, trunkTip, trunkBase string) ([]LFSObjectRef, error) {
	blobs := map[string]bool{}
	for _, args := range [][]string{
		{"log", "--format=", "--raw", "--no-abbrev", "-z", "--no-renames", head, "^" + trunkTip},
		{"diff-tree", "-r", "--raw", "--no-abbrev", "-z", "--no-renames", trunkBase, head},
	} {
		var out bytes.Buffer
		if err := s.run(ctx, nil, &out, args...); err != nil {
			return nil, err
		}
		if err := parseRawDiff(out.String(), blobs); err != nil {
			return nil, err
		}
	}
	paths := slices.Sorted(maps.Keys(blobs))
	var lfsPaths []string
	for _, key := range paths {
		p, _, _ := strings.Cut(key, "\x00")
		lfsPaths = append(lfsPaths, p)
	}
	lfsPaths = slices.Compact(lfsPaths)
	attrs, err := s.filterAttr(ctx, lfsPaths)
	if err != nil {
		return nil, err
	}
	var oids []string
	var candidates [][2]string
	for _, key := range paths {
		p, oid, _ := strings.Cut(key, "\x00")
		if attrs[p] == lfsFilter {
			candidates = append(candidates, [2]string{p, oid})
			oids = append(oids, oid)
		}
	}
	slices.Sort(oids)
	pointers, err := s.readPointers(ctx, slices.Compact(oids))
	if err != nil {
		return nil, err
	}
	var refs []LFSObjectRef
	for _, c := range candidates {
		if ptr, ok := pointers[c[1]]; ok {
			refs = append(refs, LFSObjectRef{Path: c[0], OID: ptr.OID, Size: ptr.Size})
		}
	}
	return refs, nil
}

func parseRawDiff(out string, blobs map[string]bool) error {
	tokens := strings.Split(out, "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := strings.Trim(tokens[i], "\n")
		if tok == "" {
			continue
		}
		if !strings.HasPrefix(tok, ":") || i+1 >= len(tokens) {
			return fmt.Errorf("raw diff record %q", tok)
		}
		i++
		f := strings.Fields(tok)
		if len(f) != 5 {
			return fmt.Errorf("raw diff record %q", tok)
		}
		mode, oid := f[1], f[3]
		if (mode == "100644" || mode == "100755") && strings.Trim(oid, "0") != "" {
			blobs[tokens[i]+"\x00"+oid] = true
		}
	}
	return nil
}
