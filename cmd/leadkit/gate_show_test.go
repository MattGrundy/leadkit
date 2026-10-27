package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGateAs runs the real binary against a SHARED throwaway HOME (runCLI makes a fresh one per
// call, which cannot show a record written by an earlier call) from a cwd that is NOT the repo.
func runGateAs(t *testing.T, bin, home string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = home // never the repo being asked about: the read must not depend on cwd
	cmd.Env = sandboxedEnv(home, nil)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %v: %v", args, err)
	return "", -1
}

func listTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			got[p] = true
		}
		return nil
	})
	return got
}

// TestCLI_GateShow_ReadsARecordWithoutWritingOne_Ticket91Tripwire is the scenario behind the verb: a reviewer
// needs to know whether code-review has recorded at a sha. `gate show` must answer from any cwd,
// say MISSING for the unrecorded layer, and leave the records directory exactly as it found it.
func TestCLI_GateShow_ReadsARecordWithoutWritingOne_Ticket91Tripwire(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := gitRepoNoCommits(t, "git@example.com:org/shown.git")
	profile := filepath.Join(repo, "leadkit.yaml")
	// The seeded profile names its own records_dir. `gate record` writes to the TARGET's
	// records_dir, so `gate show` must read through targetGateResolver too: reading the default
	// dir reports MISSING for a record that exists. Do not drop records_dir from this profile.
	recordsDir := filepath.Join(t.TempDir(), "records")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nrecords_dir: "+recordsDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := scopeTestFakeSHA

	// Seed through the real verb: code-review ready, critique not yet.
	cmd := exec.Command(bin, "gate", "record", "--repo", repo, "--sha", sha, "--layer", "code-review", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1")
	cmd.Dir = home
	cmd.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed record: %v\n%s", err, out)
	}

	before := listTree(t, home)
	out, code := runGateAs(t, bin, home, "gate", "show", "--repo", repo, "--sha", sha)
	if code != 0 {
		t.Fatalf("gate show exited %d:\n%s", code, out)
	}
	for _, want := range []string{"code-review", "ready", "by cr-1", "critique", "MISSING", "next to record: critique"} {
		if !strings.Contains(out, want) {
			t.Errorf("gate show output lacks %q:\n%s", want, out)
		}
	}
	after := listTree(t, home)
	if len(after) != len(before) {
		t.Errorf("gate show changed the filesystem under HOME: %d paths before, %d after", len(before), len(after))
	}

	js, code := runGateAs(t, bin, home, "gate", "show", "--repo", repo, "--sha", sha, "--json")
	if code != 0 || !strings.Contains(js, `"chain_satisfied": false`) || !strings.Contains(js, `"next": "critique"`) {
		t.Errorf("--json: exit %d\n%s", code, js)
	}
}

func TestCLI_GateShow_ErrorsRatherThanAnsweringForAnUnreadableTarget(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	out, code := runGateAs(t, bin, home, "gate", "show", "--repo", filepath.Join(home, "nope"), "--sha", scopeTestFakeSHA)
	if code != 2 || !strings.Contains(out, "bad repo") {
		t.Errorf("a missing repo must exit 2 with a reason, got %d:\n%s", code, out)
	}
	out, code = runGateAs(t, bin, home, "gate", "show")
	if code != 2 {
		t.Errorf("no repo, no sha, cwd not a repo: must exit 2 (not 'allow'), got %d:\n%s", code, out)
	}
}
