package worktree

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func oid(c byte) string { return strings.Repeat(string(c), 40) }

func art(c byte, size int64, media Media) ArtifactRef {
	return ArtifactRef{Digest: "sha256:" + strings.Repeat(string(c), 64), Size: size, Media: media}
}

func sealed(t *testing.T, s Snapshot) Snapshot {
	t.Helper()
	d, err := s.ContentDigest()
	if err != nil {
		t.Fatalf("ContentDigest: %v", err)
	}
	s.Digest = d
	return s
}

func validSnapshot(t *testing.T) Snapshot {
	t.Helper()
	blob := art('b', 3, MediaBlob)
	file := art('c', 5, MediaFile)
	lfsOID := strings.Repeat("d", 64)
	return sealed(t, Snapshot{
		Schema: SnapshotSchema,
		Source: "host-a",
		Worktree: Worktree{
			ID: worktreeID("https://example.com/r.git", "/src/r", 7), Origin: "https://example.com/r.git", Relpath: "r", Trunk: "main",
			Root: "/src/r", GitDir: "/src/r/.git", CommonDir: "/src/r/.git", Kind: KindGit,
			Branch: "feat", Head: oid('1'), Incarnation: 7,
		},
		CapturedAt:   time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		ObjectFormat: "sha1",
		Head:         Head{Commit: oid('1'), Branch: "feat", TrunkTip: oid('2'), TrunkBase: oid('2'), Ahead: 1},
		History:      []Bundle{{Artifact: art('a', 100, MediaBundle), Tip: oid('1'), Prerequisites: []string{oid('2')}}},
		Requires:     []string{oid('2')},
		Index: []IndexEntry{
			{Path: "a.txt", Mode: "100644", OID: oid('3'), Blob: &blob},
			{Path: "gone.txt", Mode: ""},
		},
		Files: []FileEntry{
			{Path: "a.txt", Kind: FileRegular, Content: &file},
			{Path: "dir/new file\n.txt", Kind: FileRegular, Content: &file, Untracked: true},
			{Path: "old.txt", Kind: FileDeleted},
		},
		IntentToAdd: []string{"ita.txt"},
		LFSObjects:  []LFSObject{{OID: lfsOID, Size: 9, Artifact: ArtifactRef{Digest: "sha256:" + lfsOID, Size: 9, Media: MediaLFSObject}}},
		LFS:         &LFSInfo{Objects: []LFSObjectRef{{Path: "assets/x.bin", OID: lfsOID, Size: 9}}, Remote: "origin"},
		Complete:    true,
	})
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	s := validSnapshot(t)
	b, err := Encode(s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	again, err := Encode(s)
	if err != nil || !bytes.Equal(b, again) {
		t.Fatalf("Encode is not deterministic: %v", err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("Decode(Encode(s)) = %+v, want %+v", got, s)
	}

	later := s
	later.CapturedAt = s.CapturedAt.Add(time.Hour)
	if d, _ := later.ContentDigest(); d != s.Digest {
		t.Fatalf("ContentDigest changed with CapturedAt: %s vs %s", d, s.Digest)
	}
	edited := s
	edited.Files = append([]FileEntry(nil), s.Files...)
	edited.Files[0].Content = &ArtifactRef{Digest: "sha256:" + strings.Repeat("f", 64), Size: 5, Media: MediaFile}
	if d, _ := edited.ContentDigest(); d == s.Digest {
		t.Fatal("ContentDigest unchanged after a file content change")
	}

	incomplete := s
	incomplete.Omitted = []Omission{{Path: "nested", Reason: OmitNestedRepo}, {Path: "vendor/sub", Reason: OmitSubmodule}}
	incomplete.Complete = false
	if _, err := Encode(sealed(t, incomplete)); err != nil {
		t.Fatalf("Encode incomplete snapshot: %v", err)
	}

	shared := s
	sameBytes := ArtifactRef{Digest: s.Index[0].Blob.Digest, Size: s.Index[0].Blob.Size, Media: MediaFile}
	shared.Files = append(append([]FileEntry(nil), s.Files...), FileEntry{Path: "same-as-staged.txt", Kind: FileRegular, Content: &sameBytes, Untracked: true})
	shared.JJ = &JJ{Workspace: "default", ChangeID: strings.Repeat("k", 32), WorkingCopyCommit: oid('6')}
	if _, err := Encode(sealed(t, shared)); err != nil {
		t.Fatalf("Encode snapshot sharing one digest across media: %v", err)
	}
}

func TestEncodeDecodeRejectsInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Snapshot)
		reseal bool
		want   string
	}{
		{"parent traversal", func(s *Snapshot) { s.Files[0].Path = "../x" }, true, `".." component`},
		{"absolute path", func(s *Snapshot) { s.Files[0].Path = "/etc/passwd" }, true, "not clean and relative"},
		{"inner traversal", func(s *Snapshot) { s.Files[0].Path = "a/../b" }, true, "not clean and relative"},
		{"double slash", func(s *Snapshot) { s.Files[0].Path = "a//b" }, true, "not clean and relative"},
		{"trailing slash", func(s *Snapshot) {
			s.Omitted, s.Complete = []Omission{{Path: "nested/", Reason: OmitNestedRepo}}, false
		}, true, "not clean and relative"},
		{"dot git", func(s *Snapshot) { s.Files[0].Path = ".git/config" }, true, `".git" component`},
		{"case-folded dot git", func(s *Snapshot) { s.Index[1].Path = "sub/.GIT/hooks" }, true, `".GIT" component`},
		{"hfs-ignorable dot git", func(s *Snapshot) { s.Files[0].Path = ".g\u200cit/config" }, true, "component"},
		{"trailing-dot dot git", func(s *Snapshot) { s.IntentToAdd = []string{".git."} }, true, "component"},
		{"lfs ref dot git", func(s *Snapshot) { s.LFS.Objects[0].Path = ".git/lfs/x" }, true, "component"},
		{"NUL", func(s *Snapshot) { s.Files[0].Path = "a\x00b" }, true, "contains NUL"},
		{"invalid UTF-8", func(s *Snapshot) { s.Files[0].Path = "a\xffb" }, true, "not valid UTF-8"},
		{"empty path", func(s *Snapshot) { s.Files[0].Path = "" }, true, "empty path"},
		{"unsorted files", func(s *Snapshot) { s.Files[0], s.Files[1] = s.Files[1], s.Files[0] }, true, "files not strictly sorted"},
		{"duplicate index path", func(s *Snapshot) { s.Index[1].Path = s.Index[0].Path }, true, "index not strictly sorted"},
		{"stale digest", func(s *Snapshot) { s.Files[0].Executable = true }, false, "digest"},
		{"schema", func(s *Snapshot) { s.Schema = "v0" }, true, "schema"},
		{"complete with omission", func(s *Snapshot) { s.Omitted = []Omission{{Path: "n", Reason: OmitNestedRepo}} }, true, "complete=true with 1 omissions"},
		{"size omission is not a reason", func(s *Snapshot) { s.Omitted, s.Complete = []Omission{{Path: "big", Reason: "too-large"}}, false }, true, `reason "too-large"`},
		{"malformed digest", func(s *Snapshot) { s.History[0].Artifact.Digest = "sha256:xyz" }, true, "malformed digest"},
		{"file content with blob media", func(s *Snapshot) {
			s.Files[0].Content = &ArtifactRef{Digest: s.Index[0].Blob.Digest, Size: 3, Media: MediaBlob}
		}, true, "media"},
		{"staged blob missing", func(s *Snapshot) { s.Index[0].Blob = nil }, true, "missing blob"},
		{"removal with content", func(s *Snapshot) { s.Index[1].OID = oid('4') }, true, "removal carries content"},
		{"deletion with content", func(s *Snapshot) { s.Files[2].Content = s.Files[0].Content }, true, "deletion carries content"},
		{"lfs artifact digest mismatch", func(s *Snapshot) { s.LFSObjects[0].Artifact.Digest = "sha256:" + strings.Repeat("0", 64) }, true, "does not match oid"},
		{"lfs ref to unshipped object", func(s *Snapshot) { s.LFS.Objects[0].OID = strings.Repeat("0", 64) }, true, "is not shipped"},
		{"shipped lfs object unreferenced", func(s *Snapshot) { s.LFS.Objects = nil }, true, "1 shipped lfs objects, 0 referenced"},
		{"lfs objects without lfs info", func(s *Snapshot) { s.LFS = nil }, true, "without lfs info"},
		{"sha1 ids under sha256 format", func(s *Snapshot) { s.ObjectFormat = "sha256" }, true, "not a sha256 object id"},
		{"ahead without history", func(s *Snapshot) { s.History = nil }, true, "head.ahead 1 with 0 history links"},
		{"history not ending at head", func(s *Snapshot) { s.History[0].Tip = oid('5') }, true, "want head"},
		{"requires without history", func(s *Snapshot) { s.History, s.Head.Ahead = nil, 0 }, true, "without history"},
		{"bad worktree kind", func(s *Snapshot) { s.Worktree.Kind = "svn" }, true, `kind "svn"`},
		{"one digest at two sizes", func(s *Snapshot) {
			s.Files[0].Content = &ArtifactRef{Digest: s.Index[0].Blob.Digest, Size: 4, Media: MediaFile}
		}, true, "declared at sizes 3 and 4"},
		{"bundle digest reused by an lfs object at another size", func(s *Snapshot) {
			s.History[0].Artifact.Digest = s.LFSObjects[0].Artifact.Digest
		}, true, "declared at sizes 100 and 9"},
		{"worktree id unrelated to its fields", func(s *Snapshot) { s.Worktree.ID = strings.Repeat("0", 32) }, true, "does not derive from origin, root, and incarnation"},
		{"worktree id from another incarnation", func(s *Snapshot) { s.Worktree.Incarnation = 8 }, true, "does not derive from origin, root, and incarnation"},
		{"worktree head not an object id", func(s *Snapshot) { s.Worktree.Head = "not-an-object-id" }, true, `worktree.head "not-an-object-id" is not a sha1 object id`},
		{"worktree head of another format", func(s *Snapshot) { s.Worktree.Head = strings.Repeat("1", 64) }, true, "worktree.head"},
		{"jj working copy commit not an object id", func(s *Snapshot) {
			s.JJ = &JJ{Workspace: "default", ChangeID: strings.Repeat("k", 32), WorkingCopyCommit: "xyz"}
		}, true, `jj.working_copy_commit "xyz" is not a sha1 object id`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot(t)
			s.Files = append([]FileEntry(nil), s.Files...)
			s.Index = append([]IndexEntry(nil), s.Index...)
			tc.mutate(&s)
			if tc.reseal {
				s = sealed(t, s)
			}
			_, err := Encode(s)
			if !errors.Is(err, ErrInvalidSnapshot) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Encode error = %v, want ErrInvalidSnapshot containing %q", err, tc.want)
			}
			if tc.name == "invalid UTF-8" {
				return
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := Decode(raw); !errors.Is(err, ErrInvalidSnapshot) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Decode error = %v, want ErrInvalidSnapshot containing %q", err, tc.want)
			}
		})
	}
}

func TestDecodeRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	b, err := Encode(validSnapshot(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for name, raw := range map[string][]byte{
		"unknown field": append([]byte(`{"extra":1,`), b[1:]...),
		"trailing data": append(append([]byte(nil), b...), []byte(` {}`)...),
	} {
		if _, err := Decode(raw); !errors.Is(err, ErrInvalidSnapshot) {
			t.Errorf("%s: Decode error = %v, want ErrInvalidSnapshot", name, err)
		}
	}
}

func TestArtifactsAndSummary(t *testing.T) {
	s := validSnapshot(t)
	want := []ArtifactRef{art('a', 100, MediaBundle), art('b', 3, MediaBlob), art('c', 5, MediaFile), s.LFSObjects[0].Artifact}
	if got := s.Artifacts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Artifacts() = %v, want %v", got, want)
	}
	wantSum := Summary{
		WorktreeID: s.Worktree.ID, Branch: "feat", Head: oid('1'), Digest: s.Digest, CapturedAt: s.CapturedAt,
		Ahead: 1, Staged: 2, Unstaged: 2, Untracked: 1, Omitted: 0, Bytes: 117, Complete: true,
	}
	if got := s.Summary(); got != wantSum {
		t.Fatalf("Summary() = %+v, want %+v", got, wantSum)
	}
}

func TestErrorsAreMatchable(t *testing.T) {
	var partial *PartialError
	if err := error(&PartialError{Remaining: []string{"a", "b"}, Reason: PartialNewBytes}); !errors.As(err, &partial) || err.Error() != "partial worktree capture (max-new-bytes): 2 paths remaining" {
		t.Fatalf("PartialError = %v", err)
	}
	var missing *MissingLFSError
	err := error(&MissingLFSError{Objects: []LFSObjectRef{{Path: "a.bin", OID: "abc", Size: 1}}})
	if !errors.As(err, &missing) || err.Error() != "lfs objects missing from the local store: a.bin@abc" {
		t.Fatalf("MissingLFSError = %v", err)
	}
	var deferred *DeferredError
	if err := error(&DeferredError{Reason: "merge in progress"}); !errors.As(err, &deferred) || err.Error() != "worktree capture deferred: merge in progress" {
		t.Fatalf("DeferredError = %v", err)
	}
}
