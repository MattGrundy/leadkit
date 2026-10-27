package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// handMutant cuts a mutant by hand: the first old in file, replaced by new. The generator will not
// produce a mutant that fails to compile or changes nothing on demand, and these are the cases the
// harness must classify.
func handMutant(t *testing.T, root, file, old, new string) Mutant {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(src), old)
	if i < 0 {
		t.Fatalf("%q is not in %s", old, file)
	}
	return Mutant{File: file, Op: "hand", Line: 1 + strings.Count(string(src[:i]), "\n"), Before: old, After: new, start: i, end: i + len(old)}
}

func resolved(t *testing.T, root string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Real go, no fakes: each mutant goes through the production testMutant. Between them the cases
// cannot all be passed by a classifier that answers one thing for everything.
func TestMutantOutcomes_EachCategoryIsReportedAsItself(t *testing.T) {
	root := resolved(t, fixture(t, weakTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	tmp := t.TempDir()
	for i, c := range []struct {
		name   string
		m      Mutant
		want   Status
		whyHas string
		prep   func()
	}{
		// "declared and not used": the failure ticket 149 was opened for.
		{name: "invalid", m: handMutant(t, root, "p/p.go", "return true", "unused := 1"), want: Invalid, whyHas: "declared and not used"},
		{name: "survivor", m: handMutant(t, root, "p/p.go", "n > 10", "n > 10 && true"), want: Survived},
		{name: "killed", m: handMutant(t, root, "p/p.go", "n > 10", "n < 10"), want: Killed},
		{name: "not applied: the edit changes nothing", m: handMutant(t, root, "p/p.go", "n > 10", "n > 10"), want: NotApplied, whyHas: "byte-identical"},
		{
			// Cut from the file as it is now; then the file moves, so the offsets point at other text.
			// Last, because it rewrites the file.
			name: "not applied: the file moved since the mutant was cut", m: handMutant(t, root, "p/p.go", "n > 10", "n < 10"),
			want: NotApplied, whyHas: "stale",
			prep: func() {
				if err := os.WriteFile(filepath.Join(root, "p/p.go"), []byte("package p\n\n// moved\n"+strings.TrimPrefix(fixtureSrc, "package p\n")), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		if c.prep != nil {
			c.prep()
		}
		st, why := testMutant(o, tmp, i, c.m, []string{"./p"})
		if st != c.want {
			t.Errorf("%s: got %q, want %q (%s)", c.name, st, c.want, why)
		}
		if c.whyHas != "" && !strings.Contains(why, c.whyHas) {
			t.Errorf("%s: the reason must say %q, got %q", c.name, c.whyHas, why)
		}
	}
}

// A generated mutant that cannot compile, through Run: it must be reported INVALID, fail the run,
// and appear in no other column.
func invalidFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n",
		// Removing the only call leaves "os" imported and not used.
		"p/p.go":         "package p\n\nimport \"os\"\n\nfunc Clean(path string) { os.Remove(path) }\n",
		"p/p_test.go":    "package p\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n\t\"testing\"\n)\n\nfunc TestClean(t *testing.T) {\n\tf := filepath.Join(t.TempDir(), \"x\")\n\tif err := os.WriteFile(f, nil, 0o644); err != nil {\n\t\tt.Fatal(err)\n\t}\n\tClean(f)\n\tif _, err := os.Stat(f); err == nil {\n\t\tt.Fatal(\"not removed\")\n\t}\n}\n",
		"p/accepted.txt": "",
	})
	ms, err := GeneratePackage(root, "p")
	if err != nil || len(ms) != 1 {
		t.Fatalf("the fixture must generate exactly the one removable call: %v %v", ms, err)
	}
	return root
}

func TestRun_AGeneratedMutantThatDoesNotCompileIsInvalidAndFailsTheRun(t *testing.T) {
	root := invalidFixture(t)
	ok, out := runWith(t, root, Options{Pkgs: []string{"p"}, Jobs: 1})
	if ok {
		t.Fatalf("an invalid mutant must fail the run:\n%s", out)
	}
	if !strings.Contains(out, "1 INVALID mutant(s)") || !strings.Contains(out, "imported and not used") {
		t.Errorf("the invalid mutant must be named, with the compiler's reason:\n%s", out)
	}
	if want := "1 mutants = 0 killed + 0 survived + 0 not-covered + 0 hung + 0 timed-out + 1 INVALID + 0 NOT-APPLIED + 0 BROKEN"; !strings.Contains(out, want) {
		t.Errorf("the one mutant must be counted as invalid and nothing else; want %q in:\n%s", want, out)
	}
}

// A file the build excludes cannot be mutated into anything; its mutants must not read as survivors.
func TestRun_AMutantInAFileTheBuildExcludesIsNotApplied(t *testing.T) {
	root := fixture(t, strongTest)
	write(t, root, map[string]string{
		"p/never.go": "//go:build never_built\n\npackage p\n\nfunc G(a, b int) bool { return a > b }\n",
	})
	ok, out := runWith(t, root, Options{Pkgs: []string{"p"}, Jobs: 2})
	if ok {
		t.Fatalf("mutants that were never built must fail the run:\n%s", out)
	}
	if !strings.Contains(out, "NOT-APPLIED mutant(s)") || !strings.Contains(out, "p/never.go") || !strings.Contains(out, "excluded from this build") {
		t.Errorf("the excluded file's mutants must be named as not applied:\n%s", out)
	}
	if strings.Contains(out, "surviving mutant") {
		t.Errorf("not-applied must not be filed as a survivor:\n%s", out)
	}
}

func TestRun_NoMutantRunsBeforeTheBaselineIsGreen(t *testing.T) {
	root := fixture(t, strongTest)
	f := &fakeTester{canary: Invalid, narrow: Killed, wide: Killed}
	var out bytes.Buffer
	_, err := Run(Options{Root: root, Pkgs: []string{"p"}, Jobs: 1, Wide: true, AcceptedFile: "p/accepted.txt", Out: &out, test: f.test,
		cover: func(Options) (map[string]map[int]bool, error) { return nil, os.ErrInvalid }})
	if err == nil {
		t.Fatal("a red baseline must stop the run")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing, not even the canary, may run against a red baseline; %d runs happened", len(f.calls))
	}

	f = &fakeTester{canary: Invalid, base: Killed, narrow: Killed, wide: Killed}
	if _, _, err := runFake(t, root, f, true, "p/accepted.txt"); err == nil {
		t.Fatal("red importers must stop the run")
	}
	if len(f.calls) > 2 {
		t.Errorf("only the canary and the baseline may run before the importers are known green, got %d runs", len(f.calls))
	}
}

// The harness cannot do harm to the tree it reads: every mutant lives in a temp overlay. This is
// evidence, not proof: it compares the END state of two synthetic fixtures (so a write that was
// later restored would pass), and the property itself is structural (Run never opens a file under
// Root for writing). What it does catch is a path that leaves something behind, including a new
// directory or a changed mode, on the survivor, invalid and wide paths.
func TestRun_LeavesTheMutatedTreeByteIdentical(t *testing.T) {
	for name, root := range map[string]string{
		"survivors, kills and the importers": resolved(t, fixture(t, weakTest)),
		"an invalid mutant":                  resolved(t, invalidFixture(t)),
	} {
		before := treeHash(t, root)
		runWith(t, root, Options{Pkgs: []string{"p"}, Jobs: 2, Wide: true})
		if after := treeHash(t, root); after != before {
			t.Errorf("%s: a run changed the tree it mutates (files, directories or modes)", name)
		}
	}
}

func TestReport_AnOutcomeItCannotCountIsAnErrorNotADroppedMutant(t *testing.T) {
	var out bytes.Buffer
	_, err := report(Options{Out: &out, AcceptedFile: "none.txt", Root: t.TempDir()}, []Result{
		{Mutant: Mutant{File: "p/p.go", Func: "F", Op: "operator", Before: "a", After: "b"}, Status: ""},
	})
	if err == nil {
		t.Errorf("a mutant with no outcome must not vanish from the report:\n%s", out.String())
	}
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runWith(t *testing.T, root string, o Options) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	o.Root, o.AcceptedFile, o.Out = root, "p/accepted.txt", &out
	ok, err := Run(o)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return ok, out.String()
}

// treeHash covers every entry, directories included, with its mode.
func treeHash(t *testing.T, root string) [32]byte {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			names = append(names, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fi, err := os.Lstat(n)
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(n + " " + fi.Mode().String() + "\n"))
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			h.Write(b)
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// The importers' baseline is one run of every importer's suite together; on a loaded machine it
// outlasts the limit a single mutant's narrow run gets, and a timeout there would stop the run as
// "red" when the machine was only slow.
func TestRun_TheImporterBaselineGetsTheLongerLimit(t *testing.T) {
	f := &seqTester{script: func(_ int, m Mutant) Status {
		if m.Op == baselineOp {
			return Survived
		}
		return Killed
	}}
	if _, _, err := runSeqWith(t, fixture(t, strongTest), f, Options{Jobs: 1, Wide: true}); err != nil {
		t.Fatal(err)
	}
	found := false
	for key, limits := range f.limits {
		if strings.Contains(key, ": "+baselineOp+": ") {
			found = true
			if len(limits) != 1 || limits[0] != settleFactor*testTimeout {
				t.Errorf("the baseline ran with limits %v, want one run at %s", limits, settleFactor*testTimeout)
			}
		}
	}
	if !found {
		t.Error("no baseline run happened")
	}
}

// An operator that cannot produce a buildable mutant at some site is accepted with a reason, like a
// survivor. That silences the failure and nothing else: the mutant is still invalid, still in no
// score, still never a kill; and the entry goes stale when the mutant stops being invalid.
func TestRun_AnAcceptedInvalidMutantStaysInvalidAndAStaleEntryFails(t *testing.T) {
	root := invalidFixture(t)
	ms, _ := GeneratePackage(root, "p")
	write(t, root, map[string]string{"p/accepted.txt": ms[0].Key() + " -- removing the only os use orphans the import\n"})
	ok, out := runWith(t, root, Options{Pkgs: []string{"p"}, Jobs: 1})
	if !ok {
		t.Fatalf("an accepted invalid mutant must not fail the run:\n%s", out)
	}
	if want := "1 mutants = 0 killed + 0 survived + 0 not-covered + 0 hung + 0 timed-out + 1 INVALID + 0 NOT-APPLIED + 0 BROKEN"; !strings.Contains(out, want) {
		t.Errorf("accepting must not turn the invalid mutant into anything else; want %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "1 INVALID mutant(s) accepted") {
		t.Errorf("an accepted invalid mutant must still be listed:\n%s", out)
	}
	write(t, root, map[string]string{"p/accepted.txt": ms[0].Key() + " -- ok\nnot a mutant -- gone\n"})
	if ok, out := runWith(t, root, Options{Pkgs: []string{"p"}, Jobs: 1}); ok || !strings.Contains(out, "match no surviving mutant") {
		t.Errorf("a stale entry must fail the run even when the real one is accepted:\n%s", out)
	}
}

// A full disk or a broken build cache fails the go command the same way for every mutant. Such a
// failure says nothing about the tests, so a mutant that the tests WOULD kill must not be reported
// as killed (or as anything that counts) while the environment is broken.
func TestMutantOutcomes_ABrokenEnvironmentIsNeverAKill(t *testing.T) {
	root := resolved(t, fixture(t, weakTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	m := handMutant(t, root, "p/p.go", "n > 10", "n < 10")
	if st, why := testMutant(o, t.TempDir(), 0, m, []string{"./p"}); st != Killed {
		t.Fatalf("control: with a working environment this mutant is killed, got %q (%s)", st, why)
	}

	// The temp dir the mutant is written to cannot be written: the edit never lands.
	missing := filepath.Join(t.TempDir(), "gone", "deeper")
	if st, _ := testMutant(o, missing, 1, m, []string{"./p"}); st != NotApplied {
		t.Errorf("an unwritable overlay dir must read as not-applied, got %q", st)
	}

	// The build cache cannot be created: the go command fails before compiling anything.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", filepath.Join(file, "cache"))
	st, why := testMutant(o, t.TempDir(), 2, m, []string{"./p"})
	if st == Killed || st == Survived {
		t.Errorf("a build that could not run must not be judged by the tests, got %q (%s)", st, why)
	}
	if st != Invalid {
		t.Errorf("want invalid, got %q (%s)", st, why)
	}
}

// A failure of `go test` AFTER a successful `go build` is not a kill. GOFLAGS=-vet=bogus is the
// smallest such failure: `go build` ignores the flag and `go test` rejects it for every mutant,
// including one the tests do not notice. Reading that as a kill is this ticket's defect, one stage
// later.
func TestMutantOutcomes_ATestStepFailureWithNoFailingTestIsNeverAKill(t *testing.T) {
	root := resolved(t, fixture(t, weakTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	survivor := handMutant(t, root, "p/p.go", "n > 10", "n > 10 && true")
	if st, why := testMutant(o, t.TempDir(), 0, survivor, []string{"./p"}); st != Survived {
		t.Fatalf("control: in a working environment this mutant survives, got %q (%s)", st, why)
	}

	t.Setenv("GOFLAGS", "-vet=bogus")
	st, why := testMutant(o, t.TempDir(), 1, survivor, []string{"./p"})
	if st != Broken {
		t.Errorf("a go test that fails with no test failing: got %q, want %q (%s)", st, Broken, why)
	}
	if !strings.Contains(why, "-vet") {
		t.Errorf("the reason must carry the go command's own words, got %q", why)
	}
}

// Through Run: a broken test step fails the run, is named, and is in no other column.
func TestRun_ABrokenMutantFailsTheRunAndIsCountedAsNothingElse(t *testing.T) {
	root := fixture(t, strongTest)
	f := &fakeTester{canary: Invalid, base: Survived, narrow: Broken, wide: Broken}
	var out bytes.Buffer
	ok, err := Run(Options{Root: root, Pkgs: []string{"p"}, Jobs: 1, AcceptedFile: "p/accepted.txt", Out: &out, test: f.test})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if ok {
		t.Fatalf("a broken mutant must fail the run:\n%s", s)
	}
	if !strings.Contains(s, "BROKEN mutant(s)") || !strings.Contains(s, "= 0 killed + 0 survived + 0 not-covered + 0 hung + 0 timed-out + 0 INVALID + 0 NOT-APPLIED + ") {
		t.Errorf("the broken mutants must be named and counted apart from kills:\n%s", s)
	}
	if strings.Contains(s, "surviving mutant") {
		t.Errorf("broken must not be filed as a survivor:\n%s", s)
	}
}

// The overlay is the second file apply writes; a failure there must read as not-applied, not as a
// mutant that was tested. A directory standing where the overlay goes makes only that write fail.
func TestMutantOutcomes_AnOverlayThatCannotBeWrittenIsNotApplied(t *testing.T) {
	root := resolved(t, fixture(t, weakTest))
	o := Options{Root: root, Pkgs: []string{"p"}}
	m := handMutant(t, root, "p/p.go", "n > 10", "n < 10")
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "m7.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, why := testMutant(o, tmp, 7, m, []string{"./p"})
	if st != NotApplied || !strings.Contains(why, "cannot write the overlay") {
		t.Errorf("got %q (%s), want not-applied naming the overlay", st, why)
	}
}

// A build that never produced compiler output must still say why it is invalid.
func TestCompiles_AnUndiagnosedFailureStillGivesAReason(t *testing.T) {
	root := resolved(t, fixture(t, weakTest))
	o := Options{Root: root, Pkgs: []string{"p"}}

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if ok, why := compilesWithin(ctx, o, "", "p"); ok || !strings.Contains(why, "did not finish") || !strings.Contains(why, "deadline exceeded") {
		t.Errorf("a build cut short by its deadline must say so; got ok=%v reason %q", ok, why)
	}

	t.Setenv("PATH", t.TempDir())
	if ok, why := compilesWithin(context.Background(), o, "", "p"); ok || !strings.Contains(why, "executable file not found") {
		t.Errorf("a build that could not start must say why; got ok=%v reason %q", ok, why)
	}
}
