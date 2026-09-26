package worktreetest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/yasyf/reposync/worktree"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := New()
	var hooked []worktree.ArtifactRef
	s.OnPut = func(r worktree.ArtifactRef) { hooked = append(hooked, r) }
	ref, err := s.Put(ctx, worktree.MediaFile, strings.NewReader("hello\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := worktree.ArtifactRef{Digest: "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03", Size: 6, Media: worktree.MediaFile}
	if ref != want {
		t.Fatalf("Put = %+v, want %+v", ref, want)
	}
	if _, err := s.Put(ctx, worktree.MediaFile, strings.NewReader("hello\n")); err != nil {
		t.Fatalf("re-Put: %v", err)
	}
	if s.Len() != 1 || s.PutsOf(worktree.MediaFile) != 2 || len(hooked) != 2 {
		t.Fatalf("Len %d PutsOf %d hooked %d, want 1 2 2", s.Len(), s.PutsOf(worktree.MediaFile), len(hooked))
	}

	read := func() string {
		t.Helper()
		rc, err := s.Open(ctx, ref)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(b)
	}
	if got := read(); got != "hello\n" {
		t.Fatalf("Open = %q", got)
	}
	s.Truncate(ref, 2)
	if got := read(); got != "he" {
		t.Fatalf("truncated Open = %q, want he", got)
	}
	s.Corrupt(ref)
	if got := read(); got == "he" {
		t.Fatalf("corrupt Open = %q, want altered bytes", got)
	}

	dst := New()
	if err := s.CopyTo(dst, []worktree.ArtifactRef{ref}); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if has, _ := dst.Has(ctx, []worktree.ArtifactRef{ref}); !has[0] || len(dst.Puts()) != 0 {
		t.Fatalf("CopyTo: has %v puts %d", has, len(dst.Puts()))
	}

	s.Remove(ref)
	if has, _ := s.Has(ctx, []worktree.ArtifactRef{ref}); has[0] {
		t.Fatal("Has after Remove = true")
	}
	if _, err := s.Open(ctx, ref); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open after Remove error = %v, want fs.ErrNotExist", err)
	}
	s.ResetPuts()
	if len(s.Puts()) != 0 {
		t.Fatal("ResetPuts left calls")
	}
}
