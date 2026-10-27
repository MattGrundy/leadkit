package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// survivingTestBinaries runs two small tests of this package in a child `go test`, with
// TMPDIR pointed at a directory of its own, and returns the shared-binary directories left
// in it afterwards. The child is the only way to look: the cleanup happens when the process
// exits, after every test of the parent process has finished.
func survivingTestBinaries(t *testing.T, extraEnv ...string) []string {
	t.Helper()
	tmp := t.TempDir()
	cmd := exec.Command("go", "test", "-count=1", "-run", "^(TestCLI_ConfigShow_ShowOriginAnnotatesLayers|TestCLI_QueueBackfillType_BareApplyRefusesAndNeverMutates)$", ".")
	env := []string{"TMPDIR=" + tmp, keepTestBinaryEnv + "="}
	cmd.Env = append(os.Environ(), append(env, extraEnv...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child go test failed: %v\n%s", err, out)
	}
	left, err := filepath.Glob(filepath.Join(tmp, "leadkit-guard-test-bin-*"))
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// Ticket 184: the binary the CLI tests exec was built into a raw os.MkdirTemp directory
// that nothing removed, one per `go test` of this package: 1,852 copies, 30 GB, and a disk
// full of the very tests (and mutants) that were meant to be trusted.
func TestTestBinary_IsRemovedWhenThePackageRunEnds(t *testing.T) {
	if left := survivingTestBinaries(t); len(left) != 0 {
		t.Errorf("the shared test binary survived the run: %s", strings.Join(left, ", "))
	}
}

// CONTROL for the test above: with the keep switch on, the same child leaves exactly one
// directory. Without it, "nothing survived" would also be what a child that never built
// the binary, or built it somewhere this test does not look, would show.
func TestTestBinary_IsBuiltOnceAndKeptOnRequest(t *testing.T) {
	left := survivingTestBinaries(t, keepTestBinaryEnv+"=1")
	if len(left) != 1 {
		t.Fatalf("want exactly one binary directory kept (two tests ran, one binary was built for the package run), got %d: %v", len(left), left)
	}
	if _, err := os.Stat(filepath.Join(left[0], "leadkit")); err != nil {
		t.Errorf("the kept directory holds no binary: %v", err)
	}
}
