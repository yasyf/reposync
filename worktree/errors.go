package worktree

import (
	"errors"
	"fmt"
	"strings"
)

// PartialReason names the per-capture progress budget a PartialError hit.
type PartialReason string

const (
	// PartialNewBytes means the capture read MaxNewBytes of new content.
	PartialNewBytes PartialReason = "max-new-bytes"
	// PartialEntries means the capture reached MaxEntries.
	PartialEntries PartialReason = "max-entries"
)

var (
	// ErrBusy means a live lock was held or HEAD or the index moved mid-capture:
	// retry later and keep the prior checkpoint.
	ErrBusy = errors.New("worktree busy")
	// ErrRepoAbsent means the snapshot's repository is not checked out on this host.
	ErrRepoAbsent = errors.New("repo not checked out on this host")
	// ErrDestinationExists means the recovery destination already exists.
	ErrDestinationExists = errors.New("recovery destination exists")
	// ErrPathCollision means snapshot paths collide on the destination filesystem
	// (case folding or Unicode normalization).
	ErrPathCollision = errors.New("snapshot paths collide on this filesystem")
	// ErrInvalidSnapshot means a snapshot manifest failed validation.
	ErrInvalidSnapshot = errors.New("invalid snapshot")
)

// DeferredError means the worktree is mid-operation (merge, rebase,
// cherry-pick, revert, bisect, am, sequencer, unmerged index, jj conflict or
// merge @) and cannot be captured faithfully until the user finishes it.
type DeferredError struct {
	Reason string
}

func (e *DeferredError) Error() string {
	return "worktree capture deferred: " + e.Reason
}

// PartialError means a capture hit a progress budget. The content already Put
// stays in the sink and the next capture resumes from it; no Snapshot is
// produced, so a partial capture is never a recovery point. Remaining names
// the slash-separated paths still to capture.
type PartialError struct {
	Remaining []string
	Reason    PartialReason
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("partial worktree capture (%s): %d paths remaining", e.Reason, len(e.Remaining))
}

// MissingLFSError means LFS objects the snapshot must ship (staged or
// referenced by unpublished history) are absent from the local LFS store.
type MissingLFSError struct {
	Objects []LFSObjectRef
}

func (e *MissingLFSError) Error() string {
	names := make([]string, len(e.Objects))
	for i, o := range e.Objects {
		names[i] = o.Path + "@" + o.OID
	}
	return "lfs objects missing from the local store: " + strings.Join(names, ", ")
}
