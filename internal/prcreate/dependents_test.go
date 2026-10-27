package prcreate

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- the pure decision --------------------------------------------------------------------------

func stackedInput(method string, dependents ...string) DecideMergeInput {
	in := mergeableInput()
	in.Req.Method = method
	in.DependentsOK = true
	in.Dependents = dependents
	return in
}

// THE FAILING CASE, as the decision sees it: nothing asked for a method, branches are stacked, and the
// default must not be a squash.
func TestDecideMerge_NoMethodAskedAndBranchesStacked_DefaultsToMergeAndSaysSo(t *testing.T) {
	d := DecideMerge(stackedInput("", "feat/a", "feat/b"))
	if !d.Allowed {
		t.Fatalf("a defaulted merge is allowed: %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets", "--merge", "--body", "Leadkit-Merge: pr-merge", "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("stacked branches must turn the default into a merge commit:\n got %v\nwant %v", d.Argv, want)
	}
	for _, s := range []string{"defaulted to merge", "2 branch(es)", "feat/a", "feat/b", "conflicting"} {
		if !strings.Contains(d.Reason, s) {
			t.Errorf("the verdict must say it chose the method, and why (%q): %s", s, d.Reason)
		}
	}
}

func TestDecideMerge_NothingStacked_StillDefaultsToSquash(t *testing.T) {
	d := DecideMerge(stackedInput(""))
	if !d.Allowed || !reflect.DeepEqual(d.Argv, wantSquashArgv()) {
		t.Fatalf("a leaf branch squashes: %v %s", d.Argv, d.Reason)
	}
}

func TestDecideMerge_ExplicitSquashOrRebaseOverStackedBranchesIsRefused_EvenUserApproved(t *testing.T) {
	for _, m := range []string{"squash", "rebase", "SQUASH"} {
		in := stackedInput(m, "feat/a")
		in.UserApproved = true // a human vouches for review, not for orphaning a branch
		d := DecideMerge(in)
		if d.Allowed || len(d.Argv) != 0 {
			t.Fatalf("--method %s over a stacked branch must be refused: %+v", m, d)
		}
		for _, s := range []string{"1 branch(es)", "feat/a", "--method merge", "delete them on the remote", "git fetch --prune"} {
			if !strings.Contains(d.Reason, s) {
				t.Errorf("the refusal must name the damage and the way out (%q): %s", s, d.Reason)
			}
		}
		// the remote delete comes first: fetch --prune alone only drops tracking refs already gone upstream
		if r, f := strings.Index(d.Reason, "delete them on the remote"), strings.Index(d.Reason, "git fetch --prune"); r < 0 || f < r {
			t.Errorf("the hint must delete on the remote BEFORE fetch --prune: %s", d.Reason)
		}
		if strings.Contains(d.Reason, "delete/prune") {
			t.Errorf("a bare delete/prune sends the operator to a dead end (a local delete is still refused): %s", d.Reason)
		}
	}
}

func TestDecideMerge_ExplicitMergeOverStackedBranchesProceeds(t *testing.T) {
	d := DecideMerge(stackedInput("merge", "feat/a"))
	if !d.Allowed || strings.Contains(d.Reason, "defaulted") {
		t.Fatalf("an asked-for merge is not a defaulted one: %+v", d)
	}
}

// Unknown is not none. A squash or rebase needs the answer; a merge commit orphans nothing and does not.
func TestDecideMerge_UnresolvedStack_FailsClosedForSquashAndRebaseOnly(t *testing.T) {
	for _, m := range []string{"", "squash", "rebase"} {
		in := mergeableInput()
		in.Req.Method = m
		in.DependentsOK = false
		in.DependentsReason = "base \"main\" is not a branch in this clone"
		d := DecideMerge(in)
		if d.Allowed || !strings.Contains(d.Reason, "Fail-closed") || !strings.Contains(d.Reason, "--method merge") {
			t.Errorf("method %q with an unchecked stack must refuse and name the way out: %+v", m, d)
		}
	}
	in := mergeableInput()
	in.Req.Method = "merge"
	in.DependentsOK = false
	if d := DecideMerge(in); !d.Allowed {
		t.Errorf("a merge commit needs no stack answer: %s", d.Reason)
	}
}

// The zero value of the input must refuse, so a caller that forgets to resolve the stack cannot squash.
func TestDecideMerge_ZeroValueStackIsRefusedForSquash(t *testing.T) {
	in := mergeableInput()
	in.DependentsOK = false
	if d := DecideMerge(in); d.Allowed {
		t.Fatal("an unresolved stack check must not default to a squash")
	}
}

// --- the real probe, against the shape of the incident ------------------------------------------

type dependentsFixture struct {
	dir        string
	keystone   string // tip of the PR head
	k1, k2     string
	onK1, onK3 string // branch names
}

// buildDependents makes main <- k1 <- k2 <- k3 (branch "keystone", the PR head), then branches that
// carry its commits: "on-k1" forks from the OLDEST commit and does not contain the head; "on-k3" is
// built on the head. Every commit rewrites the same file so a squash of the head conflicts with them.
func buildDependents(t *testing.T) dependentsFixture {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t")
	runGit(t, dir, "config", "user.name", "t")
	lines := []string{"root"}
	commit := func(add string) string {
		lines = append(lines, add)
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", add)
		return runGit(t, dir, "rev-parse", "HEAD")
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "root")
	runGit(t, dir, "branch", "-M", "main")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")

	runGit(t, dir, "checkout", "-q", "-b", "keystone")
	k1 := commit("k1")
	k2 := commit("k2")
	k3 := commit("k3")

	lines = []string{"root", "k1"}
	runGit(t, dir, "checkout", "-q", "-b", "on-k1", k1)
	commit("d1")

	lines = []string{"root", "k1", "k2", "k3"}
	runGit(t, dir, "checkout", "-q", "-b", "on-k3", "keystone")
	commit("e1")
	runGit(t, dir, "checkout", "-q", "main")
	return dependentsFixture{dir: dir, keystone: k3, k1: k1, k2: k2, onK1: "on-k1", onK3: "on-k3"}
}

// THE POINT: the branch forked from the oldest commit does not contain the head, so a head-anchored
// count misses it.
func TestDependents_RealGit_AnchorsOnTheOldestCommitNotTheHead(t *testing.T) {
	f := buildDependents(t)

	// what a head-anchored check would have said: only the branch built on the head
	headOnly := runGit(t, f.dir, "branch", "--contains", f.keystone, "--format=%(refname:short)")
	if got := strings.Fields(headOnly); !reflect.DeepEqual(got, []string{"keystone", "on-k3"}) {
		t.Fatalf("control: containing the head finds only the PR's own branch and on-k3: %v", got)
	}

	d, err := (ExecDependentProbe{}).Dependents(f.dir, f.keystone, "keystone", "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"on-k1", "on-k3"}; !reflect.DeepEqual(d.Branches, want) {
		t.Errorf("branches = %v, want %v (on-k1 forks from the oldest commit and does not contain the head)", d.Branches, want)
	}
	if d.Anchor != f.k1 || d.Commits != 3 {
		t.Errorf("anchor = %s commits = %d, want the oldest commit %s and 3 commits", d.Anchor, d.Commits, f.k1)
	}
}

// The premise, shown rather than assumed: squash the head into main and the branch that was NOT
// reported by a head-only check is the one left conflicting.
func TestDependents_RealGit_ASquashDoesLeaveTheDependentConflicting(t *testing.T) {
	f := buildDependents(t)
	runGit(t, f.dir, "checkout", "-q", "main")
	runGit(t, f.dir, "merge", "-q", "--squash", "keystone")
	runGit(t, f.dir, "commit", "-qm", "squash keystone")

	c := exec.Command("git", "merge", "--no-edit", f.onK1)
	c.Dir = f.dir
	out, err := c.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "CONFLICT") {
		t.Fatalf("a branch stacked on the squashed range must conflict with the squash: %v\n%s", err, out)
	}
}

func TestDependents_RealGit_RemoteCopiesAreOneBranchAndTheHeadsOwnRefsAreNotDependents(t *testing.T) {
	f := buildDependents(t)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/on-k1", f.onK1)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/keystone", "keystone")
	runGit(t, f.dir, "update-ref", "refs/remotes/fork/keystone", "keystone")
	runGit(t, f.dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/keystone")
	// another name for the very same commit changes nothing for anyone and is not a dependent
	runGit(t, f.dir, "update-ref", "refs/heads/alias-of-the-head", f.keystone)
	// a local copy of the PR branch that has moved past what the PR holds is still the PR's own branch
	runGit(t, f.dir, "branch", "-f", "keystone", "on-k3")

	d, err := (ExecDependentProbe{}).Dependents(f.dir, f.keystone, "keystone", "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"on-k1", "on-k3"}; !reflect.DeepEqual(d.Branches, want) {
		t.Errorf("branches = %v, want %v", d.Branches, want)
	}
}

// A branch that was fetched and never checked out exists only as refs/remotes/origin/<name>. The plan
// says "none among the refs this clone has fetched", so those refs must be seen, and a squash refused.
func TestDependents_RealGit_ABranchHeldOnlyAsARemoteTrackingRefIsSeenAndBlocksASquash(t *testing.T) {
	f := buildDependents(t)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/on-k1", f.onK1)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/on-k3", f.onK3)
	runGit(t, f.dir, "branch", "-D", "on-k1", "on-k3")
	if got := runGit(t, f.dir, "for-each-ref", "refs/heads/", "--format=%(refname:short)"); strings.Contains(got, "on-k") {
		t.Fatalf("precondition: no local copy of the dependents remains: %q", got)
	}

	d, err := (ExecDependentProbe{}).Dependents(f.dir, f.keystone, "keystone", "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"on-k1", "on-k3"}; !reflect.DeepEqual(d.Branches, want) {
		t.Fatalf("branches = %v, want %v (held only as remote-tracking refs)", d.Branches, want)
	}

	in := mergeableInput()
	in.Req.Method = "squash"
	in.DependentsOK, in.Dependents = true, d.Branches
	if dec := DecideMerge(in); dec.Allowed || !strings.Contains(dec.Reason, "on-k1") {
		t.Fatalf("a squash over remote-only dependents must be refused and name them: %+v", dec)
	}
}

func TestDependents_RealGit_ALeafBranchHasNone(t *testing.T) {
	f := buildDependents(t)
	runGit(t, f.dir, "branch", "-D", "on-k1", "on-k3")
	d, err := (ExecDependentProbe{}).Dependents(f.dir, f.keystone, "keystone", "main")
	if err != nil || len(d.Branches) != 0 {
		t.Fatalf("nothing is stacked on a leaf: %v %v", d.Branches, err)
	}
}

// Once the range is on the base there is nothing to rewrite, and a branch above it is merely a branch.
func TestDependents_RealGit_AnAlreadyMergedRangeHasNoAnchor(t *testing.T) {
	f := buildDependents(t)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/main", "keystone")
	d, err := (ExecDependentProbe{}).Dependents(f.dir, f.keystone, "keystone", "main")
	if err != nil || len(d.Branches) != 0 || d.Anchor != "" || d.Commits != 0 {
		t.Fatalf("an empty range has no dependents: %+v %v", d, err)
	}
}

// A head that merged another line in has two oldest commits; a branch built on either is stacked.
func TestDependents_RealGit_EveryRootOfAMergedRangeIsAnAnchor(t *testing.T) {
	f := buildDependents(t)
	write := func(name string) string {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, f.dir, "add", "-A")
		runGit(t, f.dir, "commit", "-qm", name)
		return runGit(t, f.dir, "rev-parse", "HEAD")
	}
	runGit(t, f.dir, "checkout", "-q", "-b", "side", "main")
	s1 := write("s1")
	runGit(t, f.dir, "branch", "on-s1", s1)
	runGit(t, f.dir, "checkout", "-q", "-b", "joined", "keystone")
	runGit(t, f.dir, "merge", "-q", "--no-ff", "-m", "join", "side")
	joined := runGit(t, f.dir, "rev-parse", "HEAD")

	d, err := (ExecDependentProbe{}).Dependents(f.dir, joined, "joined", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"on-s1", "on-k1", "keystone", "side"} {
		found := false
		for _, b := range d.Branches {
			found = found || b == want
		}
		if !found {
			t.Errorf("%s carries a commit of the range and must be reported: %v", want, d.Branches)
		}
	}
}

func TestDependents_RealGit_UnresolvableThingsAreErrorsNotEmptySets(t *testing.T) {
	f := buildDependents(t)
	if _, err := (ExecDependentProbe{}).Dependents(f.dir, f.keystone, "keystone", "no-such-base"); err == nil || !strings.Contains(err.Error(), "not a branch") {
		t.Errorf("an unknown base is an error: %v", err)
	}
	if _, err := (ExecDependentProbe{}).Dependents(f.dir, "no-such-head", "x", "main"); err == nil {
		t.Error("an unknown head is an error")
	}
	if _, err := (ExecDependentProbe{}).Dependents(t.TempDir(), f.keystone, "keystone", "main"); err == nil {
		t.Error("not a repository is an error")
	}
}

func TestBranchName(t *testing.T) {
	for ref, want := range map[string]string{
		"refs/heads/feat/x":          "feat/x",
		"refs/remotes/origin/feat/x": "feat/x",
		"refs/remotes/fork/feat/x":   "feat/x",
		"refs/remotes/origin/main":   "main",
		"refs/tags/v1":               "refs/tags/v1",
	} {
		if got := branchName(ref); got != want {
			t.Errorf("branchName(%q) = %q, want %q", ref, got, want)
		}
	}
}
