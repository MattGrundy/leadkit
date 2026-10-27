package prcreate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// --- the decision, with the check's result fed in ----------------------------------------------

// A gated head over ungated history is the incident: the head's own gate said yes. The zero value
// of LineageOK must refuse, so a caller that forgets to resolve it cannot silently skip the check.
func TestDecide_ALineageThatIsNotOKRefusesEvenWhenTheHeadIsGated(t *testing.T) {
	in := allowedInput()
	in.GateOK = true
	in.LineageOK = false
	in.LineageReason = "built on 1 ungated branch(es): feat/embedded-starters@abcd1234 (7 commit(s), no complete gate record)"
	d := Decide(in)
	if d.Allowed {
		t.Fatal("a gated head over an ungated stack must be refused")
	}
	for _, want := range []string{"gated but its lineage is not", "feat/embedded-starters", "7 commit(s)"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, d.Reason)
		}
	}
	if len(d.Argv) != 0 {
		t.Errorf("a refusal must carry no argv: %v", d.Argv)
	}

	in.LineageReason = ""
	if d := Decide(in); d.Allowed || !strings.Contains(d.Reason, "never checked") {
		t.Errorf("an unresolved lineage says so: allowed=%v %q", d.Allowed, d.Reason)
	}
}

func TestDecide_UserApprovedSubstitutesForLineageAsItDoesForTheGate(t *testing.T) {
	in := allowedInput()
	in.LineageOK = false
	in.UserApproved = true
	if d := Decide(in); !d.Allowed {
		t.Errorf("a human approving what lies beneath substitutes for the lineage check: %s", d.Reason)
	}
}

func TestDecideMerge_ALineageThatIsNotOKRefuses_AndUserApprovedSubstitutes(t *testing.T) {
	in := mergeableInput()
	in.LineageOK = false
	in.LineageReason = "built on 1 ungated branch(es)"
	d := DecideMerge(in)
	if d.Allowed || !strings.Contains(d.Reason, "lineage is not") {
		t.Fatalf("merge must refuse a gated HEAD over an ungated stack: %v %s", d.Allowed, d.Reason)
	}
	in.UserApproved = true
	if d := DecideMerge(in); !d.Allowed {
		t.Errorf("--user-approved substitutes: %s", d.Reason)
	}
}

// --- CheckLineage with a fake probe and records -------------------------------------------------

type fakeLineage struct {
	l   Lineage
	err error
	// covers answers LineageProbe.Covers; nil means nothing is covered.
	covers func(reviewBase, ancestor, tip string) (bool, error)
}

func (f fakeLineage) StackParents(_, _, _ string) (Lineage, error) { return f.l, f.err }

func (f fakeLineage) Covers(_, _, reviewBase, ancestor, tip string, _ []string) (bool, error) {
	if f.covers == nil {
		return false, nil
	}
	return f.covers(reviewBase, ancestor, tip)
}

type fakeRecords map[string]gate.Record

func (f fakeRecords) ReadRecord(sha string) (gate.Record, error) {
	r, ok := f[sha]
	if !ok {
		return gate.Record{}, os.ErrNotExist
	}
	return r, nil
}

func ready(layers ...string) gate.Record {
	r := gate.Record{Layers: map[string]gate.Layer{}}
	for _, l := range layers {
		r.Layers[l] = gate.Layer{Verdict: "ready"}
	}
	return r
}

func TestCheckLineage(t *testing.T) {
	parents := []StackParent{{Refs: []string{"feat/embedded-starters"}, SHA: "k7k7k7k7k7", Commits: 7}, {Refs: []string{"feat/a"}, SHA: "a1a1a1a1a1", Commits: 9}}
	both := ready("code-review", "critique")

	ok, reason := CheckLineage(fakeLineage{l: Lineage{Total: 24, Parents: parents}}, fakeRecords{"a1a1a1a1a1": both}, "", "head", "main", nil)
	if ok {
		t.Fatal("a stack with one ungated branch must refuse")
	}
	for _, want := range []string{"24 commit(s) beyond main", "feat/embedded-starters@k7k7k7k7k7 (7 commit(s), no complete gate record)", "1 ungated"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q: %s", want, reason)
		}
	}
	if strings.Contains(reason, "feat/a@") {
		t.Errorf("a gated branch must not be named as ungated: %s", reason)
	}

	ok, reason = CheckLineage(fakeLineage{l: Lineage{Total: 16, Parents: parents}}, fakeRecords{"a1a1a1a1a1": both, "k7k7k7k7k7": both}, "", "head", "main", nil)
	if !ok || !strings.Contains(reason, "each fully gated") {
		t.Errorf("a fully gated stack is fine: %v %s", ok, reason)
	}

	if ok, _ := CheckLineage(fakeLineage{l: Lineage{Total: 3}}, fakeRecords{}, "", "head", "main", nil); !ok {
		t.Error("a PR built straight on its base has nothing beneath it to be ungated")
	}

	// a parent with only ONE layer ready is not gated
	if ok, _ := CheckLineage(fakeLineage{l: Lineage{Total: 2, Parents: parents[:1]}}, fakeRecords{"k7k7k7k7k7": ready("code-review")}, "", "head", "main", nil); ok {
		t.Error("a parent missing the critique layer is not gated")
	}
	// the repo's custom required layers are honoured
	if ok, _ := CheckLineage(fakeLineage{l: Lineage{Total: 2, Parents: parents[:1]}}, fakeRecords{"k7k7k7k7k7": ready("code-review")}, "", "head", "main", []string{"code-review"}); !ok {
		t.Error("with required_gates [code-review] a code-review record is enough")
	}
	// a changes-needed verdict is not ready
	rec := gate.Record{Layers: map[string]gate.Layer{"code-review": {Verdict: "changes-needed"}, "critique": {Verdict: "ready"}}}
	if ok, _ := CheckLineage(fakeLineage{l: Lineage{Total: 2, Parents: parents[:1]}}, fakeRecords{"k7k7k7k7k7": rec}, "", "head", "main", nil); ok {
		t.Error("changes-needed is not gated")
	}
}

func TestCheckLineage_UnknownIsRefusedNotAllowed(t *testing.T) {
	if ok, reason := CheckLineage(fakeLineage{err: errors.New("base \"main\" is not a branch")}, fakeRecords{}, "", "head", "main", nil); ok || !strings.Contains(reason, "Fail-closed") {
		t.Errorf("an unreadable lineage must refuse: %v %s", ok, reason)
	}
	if ok, _ := CheckLineage(fakeLineage{}, fakeRecords{}, "", "  ", "main", nil); ok {
		t.Error("an unresolved head cannot be checked, so it must refuse")
	}
}

// --- the real probe, against the shape of the incident -------------------------------------------

// gitRepo builds main <- k1..kN (branch "keystone", NO gate record) <- a1 (branch "gated-tip", with a
// full record): the tip is gated and the commits beneath it are not.
type stackFixture struct {
	dir                 string
	main, keystone, tip string
	g                   *gate.Gate
	recordsDir          string
}

// identityGit resolves every ref to itself, so the gate can record against shas the real repo made.
type identityGit struct{}

func (identityGit) RevParse(_, ref string) (string, bool) {
	return strings.TrimSuffix(ref, "^{commit}"), true
}
func (identityGit) RemoteURL(string) string { return "" }

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func buildStack(t *testing.T) stackFixture {
	t.Helper()
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
		return runGit(t, dir, "rev-parse", "HEAD")
	}
	commit("root")
	runGit(t, dir, "branch", "-M", "main")
	main := runGit(t, dir, "rev-parse", "main")
	runGit(t, dir, "checkout", "-q", "-b", "keystone")
	commit("k1")
	commit("k2")
	keystone := commit("k3")
	runGit(t, dir, "checkout", "-q", "-b", "gated-tip")
	tip := commit("a1")
	rec := t.TempDir()
	g := &gate.Gate{RecordsDir: rec, Git: identityGit{}}
	for _, l := range []string{"code-review", "critique"} {
		if _, err := g.Record(gate.RecordOpts{SHA: tip, Layer: l, Verdict: "ready", By: "r-" + l}); err != nil {
			t.Fatal(err)
		}
	}
	return stackFixture{dir: dir, main: main, keystone: keystone, tip: tip, g: g, recordsDir: rec}
}

// THE FAILING CASE: the tip's own gate is green, and nothing looks beneath it. This is the check that
// refuses, run with real git against the exact shape (gated tip over three ungated commits).
func TestLineage_RealGit_AGatedTipOverUngatedCommitsIsRefused(t *testing.T) {
	f := buildStack(t)

	// precondition: the head's own gate IS satisfied, which is why a check of the tip alone says yes
	if rec, err := f.g.ReadRecord(f.tip); err != nil || !fullyGated(f.g, f.tip, []string{"code-review", "critique"}) {
		t.Fatalf("precondition: the tip is fully gated: %v %v", rec, err)
	}

	ok, reason := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil)
	if ok {
		t.Fatalf("the PR carries 4 commits of which 3 are ungated; it must be refused: %s", reason)
	}
	for _, want := range []string{"4 commit(s) beyond main", "keystone@" + f.keystone[:8], "3 commit(s)", "no complete gate record"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, reason)
		}
	}

	// Gating the branch beneath is the remedy the refusal names, and it clears the check.
	for _, l := range []string{"code-review", "critique"} {
		if _, err := f.g.Record(gate.RecordOpts{SHA: f.keystone, Layer: l, Verdict: "ready", By: "r-" + l}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, reason := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil); !ok {
		t.Errorf("once the branch beneath is fully gated the stack is fine: %s", reason)
	}
}

func TestLineage_RealGit_WhatIsNotAStackIsNotRefused(t *testing.T) {
	f := buildStack(t)

	// 1. a branch that forks straight off main has nothing beneath it
	runGit(t, f.dir, "checkout", "-q", "-b", "straight", "main")
	if err := os.WriteFile(filepath.Join(f.dir, "s"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.dir, "add", "-A")
	runGit(t, f.dir, "commit", "-qm", "straight")
	straight := runGit(t, f.dir, "rev-parse", "HEAD")
	ok, reason := CheckLineage(ExecLineageProbe{}, fakeRecords{}, f.dir, straight, "main", nil)
	if !ok || !strings.Contains(reason, "only commit beyond main") {
		t.Errorf("a branch off the base is not a stack: %v %s", ok, reason)
	}

	// 2. once the branch beneath is MERGED into the base it is not beneath anything
	runGit(t, f.dir, "checkout", "-q", "main")
	runGit(t, f.dir, "merge", "-q", "--no-ff", "-m", "merge keystone", "keystone")
	ok, reason = CheckLineage(ExecLineageProbe{}, fakeRecords{}, f.dir, f.tip, "main", nil)
	if !ok {
		t.Errorf("a parent already on main must not count: %s", reason)
	}

	// 3. a head sitting AT a branch tip is not its own parent
	if ok, reason := CheckLineage(ExecLineageProbe{}, fakeRecords{}, f.dir, f.tip, "main", nil); !ok {
		t.Errorf("after the merge only the head's own tip remains: %s", reason)
	}
}

func TestLineage_RealGit_RemoteTrackingRefsAndDuplicateNames(t *testing.T) {
	f := buildStack(t)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/keystone", f.keystone)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/main", f.main)
	ok, reason := CheckLineage(ExecLineageProbe{}, fakeRecords{}, f.dir, f.tip, "main", nil)
	if ok || !strings.Contains(reason, "keystone = origin/keystone") {
		t.Errorf("a published and a local ref at one sha are ONE parent, named together: %v\n%s", ok, reason)
	}
	if strings.Count(reason, "ungated branch(es)") != 1 || !strings.Contains(reason, "1 ungated") {
		t.Errorf("one sha is one branch, not two: %s", reason)
	}
}

func TestLineage_RealGit_UnresolvableThingsAreErrorsNotCleanVerdicts(t *testing.T) {
	f := buildStack(t)
	if _, err := (ExecLineageProbe{}).StackParents(f.dir, f.tip, "no-such-base"); err == nil || !strings.Contains(err.Error(), "not a branch") {
		t.Errorf("an unknown base is an error: %v", err)
	}
	if _, err := (ExecLineageProbe{}).StackParents(f.dir, "no-such-head", "main"); err == nil {
		t.Error("an unknown head is an error")
	}
	if ok, reason := CheckLineage(ExecLineageProbe{}, fakeRecords{}, f.dir, f.tip, "no-such-base", nil); ok || !strings.Contains(reason, "Fail-closed") {
		t.Errorf("must refuse: %v %s", ok, reason)
	}
}

// A failure of git is an ERROR, never "not an ancestor": reading it as "no" would drop a ref from
// the stack and let an ungated line through.
func TestIsAncestor_AFailureIsAnErrorNotAnAnswer(t *testing.T) {
	f := buildStack(t)
	if yes, err := isAncestor(f.dir, f.main, f.tip); err != nil || !yes {
		t.Fatalf("main is an ancestor of the tip: %v %v", yes, err)
	}
	if yes, err := isAncestor(f.dir, f.tip, f.main); err != nil || yes {
		t.Fatalf("the tip is not an ancestor of main (an answer, not an error): %v %v", yes, err)
	}
	if _, err := isAncestor(f.dir, "0000000000000000000000000000000000000000", f.tip); err == nil {
		t.Error("an unknown commit must be an error")
	}
	if _, err := isAncestor(t.TempDir(), f.main, f.tip); err == nil {
		t.Error("not a repository must be an error")
	}
}

// THE KNOWN GAP, written down so a green is never read as stronger than it is. The tip is gated and
// three commits beneath it are not; deleting the branch that pointed at them leaves no ref to find, and
// the check passes. It must at least not say the PR was "built directly on" the base, and it must say
// what it could not see. Closing this needs the reviewed range in the gate record.
func TestLineage_RealGit_KnownGap_CommitsWithNoBranchTipAreInvisible_AndTheMessageSaysSo(t *testing.T) {
	f := buildStack(t)
	if ok, _ := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil); ok {
		t.Fatal("control: with the keystone branch present the same PR is refused")
	}
	runGit(t, f.dir, "branch", "-D", "keystone")

	ok, reason := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil)
	if !ok {
		t.Fatalf("documenting the gap: with no ref to the three commits the check cannot see them, so it passes: %s", reason)
	}
	if strings.Contains(reason, "built directly on") {
		t.Errorf("the pass must not claim the PR is built directly on main when 3 commits are beneath it: %s", reason)
	}
	for _, want := range []string{"no branch tip beneath the head was found", "4 commit(s) beyond main", "covered only by the head's own record", "deleted or unfetched branch"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the pass must say what it could not see (%q): %s", want, reason)
		}
	}
}

// A branch published on a remote other than origin is still a branch tip beneath the head.
func TestLineage_RealGit_AnyRemotesRefsAreBranchTips(t *testing.T) {
	f := buildStack(t)
	runGit(t, f.dir, "update-ref", "refs/remotes/fork/keystone", f.keystone)
	runGit(t, f.dir, "branch", "-D", "keystone")
	ok, reason := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil)
	if ok || !strings.Contains(reason, "fork/keystone") {
		t.Errorf("a tip known only as fork/keystone must still be refused when ungated: %v %s", ok, reason)
	}
}

// Ancestry is judged against what the forge sees: origin/<base> wins over a local branch of the same
// name. A local main that has advanced past origin/main carries commits the forge does not have, so a
// PR built on it is a stack on an unpublished line and the local main is a tip beneath the head.
func TestLineage_RealGit_TheBaseIsTheRemoteTrackingRefNotTheLocalBranch(t *testing.T) {
	f := buildStack(t)
	runGit(t, f.dir, "update-ref", "refs/remotes/origin/main", f.main)
	// advance local main to the keystone (as if it had been merged locally but not pushed)
	runGit(t, f.dir, "checkout", "-q", "main")
	runGit(t, f.dir, "merge", "-q", "--ff-only", "keystone")
	runGit(t, f.dir, "branch", "-D", "keystone")

	ok, reason := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil)
	if ok || !strings.Contains(reason, "4 commit(s) beyond main") || !strings.Contains(reason, "main@") {
		t.Errorf("judged against origin/main the unpublished local main is beneath the head and ungated: %v %s", ok, reason)
	}

	// With no origin/main the local branch is the base, and the same line is simply on it.
	runGit(t, f.dir, "update-ref", "-d", "refs/remotes/origin/main")
	if ok, reason := CheckLineage(ExecLineageProbe{}, f.g, f.dir, f.tip, "main", nil); !ok {
		t.Errorf("with the local main as the base the line is on it: %s", reason)
	}
}
