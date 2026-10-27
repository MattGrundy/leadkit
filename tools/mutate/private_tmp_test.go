package main

import (
	"os"
	"path/filepath"
	"testing"
)

// leakyTest is a test that kills the n>10 mutant AND leaves a directory in the temp dir behind,
// as the CLI tests did (a 17 MB binary each, ticket 184).
const leakyTest = `package p

import (
	"os"
	"testing"
)

func TestBig(t *testing.T) {
	if _, err := os.MkdirTemp("", "leaked-by-a-test-*"); err != nil {
		t.Fatal(err)
	}
	if !Big(100) || Big(0) || Big(10) {
		t.Fatal("wrong")
	}
}
`

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// Ticket 195: a mutation run is thousands of test runs, and whatever one of them leaves in the
// temp dir outlives it. Each mutant now runs in a TMPDIR of its own that is removed when the
// mutant is judged.
func TestMutantOutcomes_NothingATestLeavesInTheTempDirOutlivesTheMutant(t *testing.T) {
	root := resolved(t, fixture(t, leakyTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	m := handMutant(t, root, "p/p.go", "n > 10", "n >= 10")
	scratch := t.TempDir() // the run's own dir, made BEFORE TMPDIR moves

	// CONTROL: run in the shared temp dir, this suite does leave a directory. Without it, an
	// empty result below would also be what a fixture that leaks nothing shows.
	shared := t.TempDir()
	t.Setenv("TMPDIR", shared)
	if st, why := runTests(o, "", []string{"./p"}); st != Survived {
		t.Fatalf("control: the unmutated leaky suite passes, got %q (%s)", st, why)
	}
	if got := entries(t, shared); len(got) == 0 {
		t.Fatal("control: the leaky test left nothing in the shared temp dir, so this test proves nothing")
	}

	// The same suite through testMutant, which both kills the mutant and leaves nothing.
	harness := t.TempDir()
	t.Setenv("TMPDIR", harness)
	if st, why := testMutant(o, scratch, 7, m, []string{"./p"}); st != Killed {
		t.Fatalf("the mutant is killed by the leaky suite, got %q (%s)", st, why)
	}
	if got := entries(t, harness); len(got) != 0 {
		t.Errorf("the mutant's tests leaked into the harness's temp dir: %v", got)
	}
	if _, err := os.Stat(filepath.Join(scratch, "tmp7")); err == nil {
		t.Error("the mutant's private TMPDIR was left behind in the run's dir")
	}
}

// The unmutated baseline runs through the same path and must not leak either.
func TestMutantOutcomes_TheBaselineRunLeavesNothingEither(t *testing.T) {
	root := resolved(t, fixture(t, leakyTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	scratch := t.TempDir()
	harness := t.TempDir()
	t.Setenv("TMPDIR", harness)
	if st, why := testMutant(o, scratch, 0, Mutant{File: "p/p.go", Op: baselineOp}, []string{"./p"}); st != Survived {
		t.Fatalf("the unmutated suite passes, got %q (%s)", st, why)
	}
	if got := entries(t, harness); len(got) != 0 {
		t.Errorf("the baseline's tests leaked into the harness's temp dir: %v", got)
	}
	if _, err := os.Stat(filepath.Join(scratch, "tmp0")); err == nil {
		t.Error("the baseline's private TMPDIR was left behind")
	}
}

// The coverage pass runs the suite too; its leaks go with the profile.
func TestCoveredLines_TheCoveragePassLeavesNothingInTheTempDir(t *testing.T) {
	root := resolved(t, fixture(t, leakyTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	harness := t.TempDir()
	t.Setenv("TMPDIR", harness)
	if _, err := coveredLines(o); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, harness); len(got) != 0 {
		t.Errorf("the coverage pass leaked into the harness's temp dir: %v", got)
	}
}

// A mutant that cannot be given its own TMPDIR is not run in the shared one, where its leaks would
// outlive it; it is the environment's failure and reads as broken, which fails the run.
func TestMutantOutcomes_NoPrivateTmpIsBrokenNotASharedTmpRun(t *testing.T) {
	root := resolved(t, fixture(t, leakyTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	missing := filepath.Join(t.TempDir(), "gone")
	st, why := testMutant(o, missing, 0, Mutant{File: "p/p.go", Op: baselineOp}, []string{"./p"})
	if st != Broken {
		t.Fatalf("want broken when the mutant's TMPDIR cannot be made, got %q (%s)", st, why)
	}
}
