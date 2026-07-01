package vcs

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsWorkingCopyContention(t *testing.T) {
	cases := []struct {
		id   string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"concurrent checkout", fmt.Errorf("jj new main: %w", errors.New("Internal error: Failed to check out commit 99366219: Concurrent checkout")), true},
		{"concurrent working copy operation", errors.New("Concurrent working copy operation. Try again."), true},
		{"failed checkout", errors.New("Internal error: Failed to check out commit deadbeef"), true},
		{"unrelated", errors.New("jj git fetch: exit status 1: network unreachable"), false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := IsWorkingCopyContention(c.err); got != c.want {
				t.Errorf("IsWorkingCopyContention(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
