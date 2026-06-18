package tui

import (
	"fmt"
	"regexp"
)

// targetRe matches an ssh target: an optional "user@" prefix followed by a host
// label of letters, digits, dots, or hyphens.
var targetRe = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9][A-Za-z0-9.-]*$`)

// validateTarget rejects an empty, whitespace-bearing, or malformed ssh target,
// accepting either "user@node" or a bare "node".
func validateTarget(s string) error {
	if s == "" {
		return fmt.Errorf("target is empty")
	}
	if !targetRe.MatchString(s) {
		return fmt.Errorf("invalid target %q: want user@node or node", s)
	}
	return nil
}
