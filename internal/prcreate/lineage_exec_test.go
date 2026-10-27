package prcreate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// StackParents must select exactly the branch tips beneath the head that are not yet on the base,
// using git's own ref filtering (--merged/--no-merged) rather than a per-ref subprocess walk.
func TestExecLineageProbe_StackParentsSelectsOnlyTipsBeneathHeadAndOffBase(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t")
	runGit(t, dir, "config", "user.name", "t")
	commit := func(msg string) string {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", msg)
		return strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	}
	commit("root")
	runGit(t, dir, "branch", "-M", "main")
	// Beneath head but already on the base: must be excluded.
	runGit(t, dir, "branch", "on-base")
	runGit(t, dir, "checkout", "-q", "-b", "stack1")
	c1 := commit("c1")
	// The same tip under a second name: must dedup into one StackParent.
	runGit(t, dir, "update-ref", "refs/remotes/origin/stack1", c1)
	runGit(t, dir, "checkout", "-q", "-b", "stacktop")
	c2 := commit("c2")
	// A branch off main that is not beneath head: must be excluded.
	runGit(t, dir, "checkout", "-q", "main")
	runGit(t, dir, "checkout", "-q", "-b", "other")
	commit("other")

	l, err := ExecLineageProbe{}.StackParents(dir, c2, "main")
	if err != nil {
		t.Fatal(err)
	}
	if l.Total != 2 {
		t.Fatalf("total = %d, want 2", l.Total)
	}
	if len(l.Parents) != 1 {
		t.Fatalf("parents = %+v, want exactly the stack1 tip", l.Parents)
	}
	p := l.Parents[0]
	if p.SHA != c1 || p.Commits != 1 || !reflect.DeepEqual(p.Refs, []string{"origin/stack1", "stack1"}) {
		t.Fatalf("parent = %+v, want sha %s, 1 commit, refs [origin/stack1 stack1]", p, c1)
	}
}

// A git call that overruns the deadline is an error (fail closed), never an empty answer.
func TestGitHelper_TimeoutFailsClosed(t *testing.T) {
	old := gitTimeout
	gitTimeout = time.Nanosecond
	defer func() { gitTimeout = old }()
	if _, err := git(t.TempDir(), "version"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if _, err := isAncestor(t.TempDir(), "a", "b"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("isAncestor must surface the timeout, got %v", err)
	}
}
