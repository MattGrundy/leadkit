package publish

import (
	"path/filepath"
	"strings"
	"testing"
)

// lying answers `rev-list` with a fixed number, standing in for a repository that holds a
// different number of commits than the tool just made.
type lying struct {
	Runner
	count string
}

func (l lying) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	for _, a := range argv {
		if a == "rev-list" {
			return []byte(l.count + "\n"), nil, nil
		}
	}
	return l.Runner.Run(argv, stdin)
}

func TestPrepare_ARepositoryWithTheWrongNumberOfCommitsFails(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	out := filepath.Join(t.TempDir(), "public")
	d := deps(t, nil)
	d.Git = lying{Runner: d.Git, count: "7"}
	rep, err := Prepare(Options{Repo: repo, Out: out}, d)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Built == nil || !strings.Contains(strings.Join(rep.Built.Problems, "\n"), "7 commits, expected 1") {
		t.Fatalf("the first release must be exactly one commit: %+v", rep.Built)
	}
}

// TestEveryRunGoesThroughTheGuard pins that the entry points apply the guard, not merely that
// the commands they happen to build are allowed: a later edit that builds a forbidden command
// must meet the refusal, not the executor.
func TestEveryRunGoesThroughTheGuard(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	rec := &recorder{}
	a, err := assess(Options{Repo: repo}, deps(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	defer a.cleanup()
	before := len(rec.calls)
	if _, _, err := a.git.Run([]string{"git", "-C", repo, "push", "origin", "main"}, nil); err == nil {
		t.Error("the runner the pipeline uses must refuse a push")
	}
	if len(rec.calls) != before {
		t.Errorf("a refused command reached the executor: %v", rec.calls[before:])
	}
}
