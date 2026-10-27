// Package gh wraps two pure text-transform helpers built on `gh`'s JSON output:
// composing a squash-merge commit message, and detecting a "[skip ci]" marker on a
// commit.
//
// CANONICAL SOURCE: both bash scripts (compose-squash-merge-message.sh and
// check-commit-skip-ci.sh) are canonical and bats-tested in the adopter's CI scripts
// repo (mirrored to ~/.claude). This package is a THIRD representation of the
// same logic and must be kept in sync by hand when either script changes. That
// sync-multiplication is a known smell (see OPERATING-MODEL.md "Per-project
// authority") — not solved here, just flagged.
package gh

import "os/exec"

// runGH executes `gh` with args and returns its stdout. A func var so tests can
// inject fixture output and never shell out to the real gh CLI.
var runGH = func(args ...string) ([]byte, error) {
	return exec.Command("gh", args...).Output()
}
