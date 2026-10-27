package prcreate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// A stack whose tip records the range it read needs no record on the branches inside that range.

// repairStack is main <- c1 (branch b1) <- c2 (branch b2, introduces a defect) <- c3 (branch tip, fixes it).
type repairStack struct {
	dir              string
	main, c1, c2, c3 string
	g                *gate.Gate
	prBase           string
}

func newRepairStack(t *testing.T) repairStack {
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
	s := repairStack{dir: dir, prBase: "main"}
	s.main = runGit(t, dir, "rev-parse", "main")
	runGit(t, dir, "checkout", "-q", "-b", "b1")
	s.c1 = commit("c1")
	runGit(t, dir, "checkout", "-q", "-b", "b2")
	s.c2 = commit("c2 introduces a defect")
	runGit(t, dir, "checkout", "-q", "-b", "tip")
	s.c3 = commit("c3 fixes the defect")
	s.g = &gate.Gate{RecordsDir: t.TempDir(), Git: identityGit{}}
	return s
}

func (s repairStack) record(t *testing.T, sha, layer, verdict, base string) {
	t.Helper()
	if _, err := s.g.Record(gate.RecordOpts{SHA: sha, Layer: layer, Verdict: verdict, By: "r-" + layer, Repo: s.dir, Base: base}); err != nil {
		t.Fatal(err)
	}
}

func (s repairStack) gateTip(t *testing.T, sha, base string) {
	t.Helper()
	s.record(t, sha, "code-review", "ready", base)
	s.record(t, sha, "critique", "ready", base)
}

func (s repairStack) check(head string) (bool, string) {
	return CheckLineage(ExecLineageProbe{}, s.g, s.dir, head, s.prBase, nil)
}

// Only the tip is gated, with a base of main: the branches beneath are covered.
func TestLineageBase_ATipRecordedAsReviewingTheWholeRangeCoversTheBranchesInsideIt(t *testing.T) {
	s := newRepairStack(t)

	// Control: no base recorded, refused.
	s.gateTip(t, s.c3, "")
	if ok, reason := s.check(s.c3); ok {
		t.Fatalf("with no recorded base the tip covers only itself, so the branches beneath refuse: %s", reason)
	}

	// The reviewers amend their ready with the base they read from (the same verdict again).
	s.gateTip(t, s.c3, s.main)
	ok, reason := s.check(s.c3)
	if !ok {
		t.Fatalf("a tip recorded as reviewing main..tip covers b1 and b2: %s", reason)
	}
	for _, want := range []string{"b1@", "b2@", "recorded as reviewed"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the pass must say what covered what (%q): %s", want, reason)
		}
	}

	// And the whole merge decision, with the head's own gate fed from the same record.
	in := mergeableInput()
	in.GateOK = fullyGated(s.g, s.c3, gate.RequiredLayers)
	in.LineageOK, in.LineageReason = ok, reason
	if d := DecideMerge(in); !d.Allowed {
		t.Errorf("merge allowed: %s", d.Reason)
	}
}

// A commit added after the tip was gated leaves the head ungated.
func TestLineageBase_ACommitAddedAfterTheTipWasGatedIsRefused(t *testing.T) {
	s := newRepairStack(t)
	s.gateTip(t, s.c3, s.main)
	runGit(t, s.dir, "checkout", "-q", "-b", "top") // keep the tip branch where it was gated
	if err := os.WriteFile(filepath.Join(s.dir, "f"), []byte("c4 ungated"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, s.dir, "add", "-A")
	runGit(t, s.dir, "commit", "-qm", "c4")
	c4 := runGit(t, s.dir, "rev-parse", "HEAD")

	in := mergeableInput()
	in.GateOK = fullyGated(s.g, c4, gate.RequiredLayers)
	in.LineageOK, in.LineageReason = s.check(c4)
	if in.GateOK {
		t.Fatal("precondition: the record is per sha, so the new head has none")
	}
	d := DecideMerge(in)
	if d.Allowed || !strings.Contains(d.Reason, "not gated") {
		t.Fatalf("an ungated commit on top of a gated tip must refuse: %v %s", d.Allowed, d.Reason)
	}

	// Moving the same branch forward leaves no branch carrying the base.
	runGit(t, s.dir, "checkout", "-q", "tip")
	runGit(t, s.dir, "branch", "-D", "top")
	if err := os.WriteFile(filepath.Join(s.dir, "f"), []byte("c4 on the tip"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, s.dir, "add", "-A")
	runGit(t, s.dir, "commit", "-qm", "c4 on tip")
	moved := runGit(t, s.dir, "rev-parse", "HEAD")
	s.gateTip(t, moved, "")
	if ok, reason := s.check(moved); ok {
		t.Errorf("c3's record named c3, and no branch is at c3 any more: %s", reason)
	}
}

func TestLineageBase_ATipWhoseRecordIsNotACompleteReadyIsRefused(t *testing.T) {
	cases := []struct {
		name string
		rec  func(t *testing.T, s repairStack)
	}{
		{"changes-needed on code-review", func(t *testing.T, s repairStack) {
			s.record(t, s.c3, "code-review", "changes-needed", "")
		}},
		{"critique changes-needed over a ready code-review with a base", func(t *testing.T, s repairStack) {
			s.record(t, s.c3, "code-review", "ready", s.main)
			s.record(t, s.c3, "critique", "changes-needed", "")
		}},
		{"partial: code-review has a base, critique was never recorded", func(t *testing.T, s repairStack) {
			s.record(t, s.c3, "code-review", "ready", s.main)
		}},
		{"partial: critique has no base, so it covers only its own sha", func(t *testing.T, s repairStack) {
			s.record(t, s.c3, "code-review", "ready", s.main)
			s.record(t, s.c3, "critique", "ready", "")
		}},
		{"a complete base-carrying record for a DIFFERENT sha", func(t *testing.T, s repairStack) {
			runGit(t, s.dir, "checkout", "-q", "-b", "side", "main")
			if err := os.WriteFile(filepath.Join(s.dir, "side"), []byte("s"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, s.dir, "add", "-A")
			runGit(t, s.dir, "commit", "-qm", "side")
			s.gateTip(t, runGit(t, s.dir, "rev-parse", "HEAD"), s.main)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newRepairStack(t)
			c.rec(t, s)
			ok, reason := s.check(s.c3)
			if ok {
				t.Fatalf("must refuse: %s", reason)
			}
			if !strings.Contains(reason, "no complete gate record") {
				t.Errorf("the refusal names what is ungated: %s", reason)
			}
		})
	}
}

// A base inside the stack leaves the commits at and below it uncovered.
func TestLineageBase_ABaseInsideTheStackDoesNotCoverWhatLiesBelowIt(t *testing.T) {
	s := newRepairStack(t)

	s.gateTip(t, s.c3, s.c2) // read c2..c3: the tip's own delta, which is what a base-less review means
	ok, reason := s.check(s.c3)
	if ok || !strings.Contains(reason, "b1@") || !strings.Contains(reason, "b2@") {
		t.Fatalf("reviewing only c2..c3 covers neither branch beneath: %v %s", ok, reason)
	}
}

// A gated branch below accounts for the commits the base excludes.
func TestLineageBase_AGatedBranchBelowAccountsForTheCommitsTheBaseExcludes(t *testing.T) {
	s := newRepairStack(t)
	s.gateTip(t, s.c3, s.c1) // read c1..c3
	ok, reason := s.check(s.c3)
	if ok || !strings.Contains(reason, "b1@") || !strings.Contains(reason, "b2@") {
		t.Fatalf("c1 is outside the read range and nothing gated accounts for it, so b1 refuses, and b2 carries c1: %v %s", ok, reason)
	}
	s.gateTip(t, s.c1, "") // b1 gated on its own
	if ok, reason := s.check(s.c3); !ok {
		t.Errorf("b1 gated itself and b2's remaining commit is inside the recorded range: %s", reason)
	}
}

// A gated branch above an ungated one covers nothing beneath it.
func TestLineageBase_AGatedBranchAboveDoesNotAccountForWhatLiesBeneathIt(t *testing.T) {
	s := newRepairStack(t)
	s.gateTip(t, s.c3, s.c2) // read c2..c3; the tip's own delta
	s.gateTip(t, s.c2, "")   // b2 gated on its own delta, above b1
	ok, reason := s.check(s.c3)
	if ok || !strings.Contains(reason, "b1@") {
		t.Errorf("b1 is beneath the gated b2 and tip, and no record covers it: %v %s", ok, reason)
	}
}

func TestLineageBase_ABranchTipBeneathTheHeadCanCoverWhatLiesBelowIt(t *testing.T) {
	s := newRepairStack(t)
	runGit(t, s.dir, "checkout", "-q", "-b", "top")
	if err := os.WriteFile(filepath.Join(s.dir, "g"), []byte("h"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, s.dir, "add", "-A")
	runGit(t, s.dir, "commit", "-qm", "head")
	head := runGit(t, s.dir, "rev-parse", "HEAD")
	s.gateTip(t, head, "")     // the head: its own delta only
	s.gateTip(t, s.c3, s.main) // the branch beneath it read the whole of main..c3
	if ok, reason := s.check(head); !ok {
		t.Errorf("b1 and b2 lie inside the range the c3 branch recorded: %s", reason)
	}
}

func TestLineageBase_ABaseThatCannotBeReadFailsClosed(t *testing.T) {
	s := newRepairStack(t)
	// A base that is a sha this clone does not have (an unfetched or rewritten history).
	s.gateTip(t, s.c3, strings.Repeat("e", 40))
	ok, reason := s.check(s.c3)
	if ok || !strings.Contains(reason, "Fail-closed") {
		t.Errorf("an unreadable base must refuse, not be read as covering or as not covering: %v %s", ok, reason)
	}
}

// The reviewers' convention is the three-dot range, so a base that has moved on since the branch forked
// (main after a sibling PR landed) still names the range from the fork point.
func TestLineageBase_ABaseThatHasMovedOnStillNamesTheForkPointRange(t *testing.T) {
	s := newRepairStack(t)
	runGit(t, s.dir, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(s.dir, "sibling"), []byte("another PR landed"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, s.dir, "add", "-A")
	runGit(t, s.dir, "commit", "-qm", "sibling PR")
	s.gateTip(t, s.c3, runGit(t, s.dir, "rev-parse", "main"))
	if ok, reason := s.check(s.c3); !ok {
		t.Errorf("main moved past the fork point; the range read is still main...tip: %s", reason)
	}
}

// A base with no history in common with the tip is not a range the tip was read over.
func TestLineageBase_ABaseWithNoCommonHistoryCoversNothing(t *testing.T) {
	s := newRepairStack(t)
	runGit(t, s.dir, "checkout", "-q", "--orphan", "unrelated")
	if err := os.WriteFile(filepath.Join(s.dir, "z"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, s.dir, "add", "-A")
	runGit(t, s.dir, "commit", "-qm", "unrelated root")
	other := runGit(t, s.dir, "rev-parse", "HEAD")
	s.gateTip(t, s.c3, other)
	if ok, reason := s.check(s.c3); ok {
		t.Errorf("a base sharing no history with the tip names no range: %s", reason)
	}
}

// With the fake probe: the covering decision is the probe's, and the record's base is what is asked about.
func TestCheckLineage_ACoveringTipWithAnyLayerMissingABaseIsNotAskedToCover(t *testing.T) {
	parents := []StackParent{{Refs: []string{"b"}, SHA: "k7k7k7k7k7", Commits: 2}}
	asked := 0
	probe := fakeLineage{l: Lineage{Total: 3, Parents: parents}, covers: func(_, _, _ string) (bool, error) { asked++; return true, nil }}
	rec := gate.Record{Layers: map[string]gate.Layer{"code-review": {Verdict: "ready", Base: "m"}, "critique": {Verdict: "ready"}}}
	if ok, _ := CheckLineage(probe, fakeRecords{"head": rec}, "", "head", "main", nil); ok {
		t.Error("a layer without a base covers nothing, whatever the probe would say")
	}
	if asked != 1 {
		t.Errorf("only the based layer is asked: %d", asked)
	}
}

// --- sibling branches, neither an ancestor of the other ---

// forkStack is main <- x (branch x, UNGATED) and main <- y (branch y, GATED reading main...y), with the
// head a merge of both. x and y are siblings: neither is an ancestor of the other.
type forkStack struct {
	dir        string
	main, x, y string
	head       string
	g          *gate.Gate
}

func newForkStack(t *testing.T) forkStack {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t")
	runGit(t, dir, "config", "user.name", "t")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", name)
	}
	write("root", "r")
	runGit(t, dir, "branch", "-M", "main")
	s := forkStack{dir: dir, g: &gate.Gate{RecordsDir: t.TempDir(), Git: identityGit{}}}
	s.main = runGit(t, dir, "rev-parse", "main")
	runGit(t, dir, "checkout", "-q", "-b", "x")
	write("x", "ungated work")
	s.x = runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-q", "-b", "y", "main")
	write("y", "gated work")
	s.y = runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-q", "-b", "merged", "main")
	runGit(t, dir, "merge", "-q", "--no-ff", "-m", "merge x and y", "x", "y")
	s.head = runGit(t, dir, "rev-parse", "HEAD")
	return s
}

func (s forkStack) gate(t *testing.T, sha, base string) {
	t.Helper()
	for _, l := range []string{"code-review", "critique"} {
		if _, err := s.g.Record(gate.RecordOpts{SHA: sha, Layer: l, Verdict: "ready", By: "r-" + l, Repo: s.dir, Base: base}); err != nil {
			t.Fatal(err)
		}
	}
}

// A gated sibling does not cover an ungated branch that is not at or below it.
func TestLineageBase_AGatedSiblingDoesNotCoverAnUnrelatedUngatedBranch(t *testing.T) {
	s := newForkStack(t)
	s.gate(t, s.head, "")  // the head's own delta only; it covers nothing beneath it
	s.gate(t, s.y, s.main) // y read main...y

	ok, reason := CheckLineage(ExecLineageProbe{}, s.g, s.dir, s.head, "main", nil)
	if ok || !strings.Contains(reason, "x@"+s.x[:12]) {
		t.Fatalf("x is ungated and y's range does not contain it: %v %s", ok, reason)
	}
	if strings.Contains(reason, "y@") {
		t.Errorf("y is gated and must not be named as ungated: %s", reason)
	}

	// the probe itself, so the guard is pinned where it lives and not only through the caller
	cov, err := (ExecLineageProbe{}).Covers(s.dir, "main", s.main, s.x, s.y, nil)
	if err != nil || cov {
		t.Errorf("Covers(x by a review of main...y) must be false: %v %v", cov, err)
	}
	cov, err = (ExecLineageProbe{}).Covers(s.dir, "main", s.main, s.y, s.y, nil)
	if err != nil || !cov {
		t.Errorf("control: y is covered by its own range main...y: %v %v", cov, err)
	}
}

// A head that recorded main as its base covers both siblings.
func TestLineageBase_AHeadThatReadTheMergeCoversBothSiblings(t *testing.T) {
	s := newForkStack(t)
	s.gate(t, s.head, s.main)
	if ok, reason := CheckLineage(ExecLineageProbe{}, s.g, s.dir, s.head, "main", nil); !ok {
		t.Errorf("x and y both lie inside main...head: %s", reason)
	}
}

// --- a layer that is not ready covers nothing, whatever it carries ------------------------------------

// Only ready layers are consulted for a base.
func TestLineageBase_ALayerThatIsNotReadyCoversNothingEvenWithABase(t *testing.T) {
	for _, verdict := range []string{"changes-needed", "needs-re-review", "", "READY", "approved"} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			s := newRepairStack(t)
			s.gateTip(t, s.c3, s.main)
			// edit the record on disk: one layer keeps its base but is not ready
			path := filepath.Join(s.g.RecordsDir, s.c3+".json")
			rec, err := s.g.ReadRecord(s.c3)
			if err != nil {
				t.Fatal(err)
			}
			l := rec.Layers["critique"]
			l.Verdict = verdict
			rec.Layers["critique"] = l
			data, err := json.MarshalIndent(rec, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if rec.Layers["critique"].Base == "" {
				t.Fatal("precondition: the edited layer still carries its base")
			}
			if ok, reason := s.check(s.c3); ok {
				t.Errorf("a layer recorded %q is not ready and its base must not count: %s", verdict, reason)
			}
		})
	}
}
