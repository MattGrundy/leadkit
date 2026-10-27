package main

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/prcreate"
)

// The plan prints what is stacked on the PR beside the method, whatever method was chosen: a merge
// commit is safe, but the reader should still see what a squash would have done.
func TestFormatStackedLine(t *testing.T) {
	d := &prcreate.Dependents{Anchor: "0123456789abcdef", Commits: 27}
	line := formatStackedLine(prcreate.DecideMergeInput{DependentsOK: true, Dependents: []string{"feat/a", "feat/b"}}, d)
	for _, want := range []string{"2 branch(es)", "feat/a, feat/b", "conflicting", "oldest of its 27 commit(s), 01234567"} {
		if !strings.Contains(line, want) {
			t.Errorf("stacked line lacks %q: %s", want, line)
		}
	}
	if line := formatStackedLine(prcreate.DecideMergeInput{DependentsOK: true}, nil); !strings.HasPrefix(line, "none") || !strings.Contains(line, "fetched") {
		t.Errorf("nothing stacked must read as none, scoped to the refs this clone has fetched: %s", line)
	}
	if line := formatStackedLine(prcreate.DecideMergeInput{DependentsReason: "base \"main\" is not a branch"}, nil); !strings.HasPrefix(line, "UNKNOWN") || !strings.Contains(line, "not a branch") {
		t.Errorf("an unchecked stack must read as unknown, never none: %s", line)
	}
}
