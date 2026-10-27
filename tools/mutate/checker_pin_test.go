package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// metaEnv is set inside the copy this file's test runs, so the copy does not run the test again.
const metaEnv = "MUTATE_CHECKER_PIN_COPY"

// The harness's own checks are the thing that makes a kill earned, so they need the same proof as
// any guard: break each one and see a test go red. This does it for real. It copies the harness to
// a scratch module, confirms the copy passes, removes one check from the copy's source, and
// requires the outcome tests there to fail with the defect the check exists to prevent. The edit
// is made in Go and verified by hash: a replace that matched nothing would leave the copy green
// and read as a check that is pinned.
func TestCheckers_MutatedOutEachTestGoesRedWithTheDefectItPrevents(t *testing.T) {
	if os.Getenv(metaEnv) != "" {
		t.Skip("inside the copy")
	}
	outcomes := `^(TestMutantOutcomes_|TestRun_AGeneratedMutantThatDoesNotCompile|TestRun_AMutantInAFileTheBuildExcludes)`
	control := t.TempDir()
	copyHarness(t, control)
	if out, err := goTest(control, outcomes); err != nil {
		t.Fatalf("the unmodified copy must pass, or a red copy proves nothing: %v\n%s", err, out)
	}
	for _, c := range []struct {
		name, file, old, new string
		// failure must appear in the copy's output: the wrong answer the removed check stops.
		failure string
	}{
		{
			name: "compile check: an invalid mutant reads as killed",
			file: "run.go",
			old:  `if ok, out := compiles(o, overlay, filepath.Dir(m.File)); !ok {`,
			new:  `if ok, out := true, ""; !ok {`,
			// The mutant does not build and `go test` fails on it. The test-step check (next case)
			// stops that being taken for a kill, so it lands as broken here: still wrong, since
			// only the compile check can say invalid, and it is what gives the compiler's reason.
			failure: `invalid: got "broken", want "invalid"`,
		},
		{
			name: "test-step check: a go test that fails with no test failing reads as killed",
			file: "run.go",
			old:  `	if testFailed(out) {`,
			new:  `	if true {`,
			// The ticket's defect one stage later: the mutant builds, `go test` fails for the
			// environment's reasons, and the failure is taken for a kill.
			failure: `a go test that fails with no test failing: got "killed", want "broken"`,
		},
		{
			name:    "no-op check: an edit that changed nothing reads as a survivor",
			file:    "run.go",
			old:     `if sha256.Sum256(src) == sha256.Sum256(orig) {`,
			new:     `if false && sha256.Sum256(src) == sha256.Sum256(orig) {`,
			failure: `not applied: the edit changes nothing: got "survived", want "not-applied"`,
		},
		{
			name:    "stale-offset check: an edit cut from other bytes is applied anyway",
			file:    "run.go",
			old:     `|| string(orig[m.start:m.end]) != m.Before {`,
			new:     `|| false {`,
			failure: `not applied: the file moved since the mutant was cut: got "invalid", want "not-applied"`,
		},
		{
			name:    "build-constraint check: a mutant in an unbuilt file reads as a survivor",
			file:    "run.go",
			old:     `if !built[m.File] {`,
			new:     `if false && !built[m.File] {`,
			failure: `not-applied must not be filed`,
		},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			copyHarness(t, dir)
			path := filepath.Join(dir, c.file)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(src), c.old); n != 1 {
				t.Fatalf("the check to remove appears %d times in %s, want exactly 1: this test is out of date with the code", n, c.file)
			}
			edited := []byte(strings.Replace(string(src), c.old, c.new, 1))
			if sha256.Sum256(edited) == sha256.Sum256(src) {
				t.Fatal("the edit changed nothing")
			}
			if err := os.WriteFile(path, edited, 0o644); err != nil {
				t.Fatal(err)
			}
			if back, _ := os.ReadFile(path); !bytes.Equal(back, edited) {
				t.Fatal("the edit was not written")
			}
			out, err := goTest(dir, outcomes)
			if err == nil {
				t.Fatalf("with the check removed the outcome tests still pass: the check is unpinned\n%s", out)
			}
			if !strings.Contains(out, c.failure) {
				t.Errorf("the copy failed, but not with the defect this check prevents (%q):\n%s", c.failure, out)
			}
		})
	}
}

func goTest(dir, run string) (string, error) {
	cmd := exec.Command("go", "test", "-count=1", "-run", run, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), metaEnv+"=1", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func copyHarness(t *testing.T, dst string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "go.mod"), []byte("module example.com/harnesscopy\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
