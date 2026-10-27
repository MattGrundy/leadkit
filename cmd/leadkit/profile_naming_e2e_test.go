package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 319, end to end: with the git pre-push hook installed, a real publish from a clone whose
// remote was renamed must not reach the remote without a review record, in either mode and with
// or without a .leadkit/ directory. This is what actually happened: a commit with no record landed
// on the remote while `leadkit push` refused the same repo.
func TestNamingE2E_ARealPushThroughThePrePushHookDoesNotReachTheRemote(t *testing.T) {
	bin := testBinary(t)
	for _, mode := range []string{"enforce", "advisory"} {
		for _, leadkit := range []bool{false, true} {
			name := mode + "/leadkit=" + map[bool]string{false: "absent", true: "present"}[leadkit]
			t.Run(name, func(t *testing.T) {
				c := namingCell{mode: mode, leadkit: leadkit, op: "rename", sameNamed: false}
				w := newNamingWorld(t, c)
				installPrePush(t, w.repo, bin)

				// Control: the same clone with its origin intact is held by the gate (no record).
				gitRun(t, w.repo, "remote", "rename", "x", "origin")
				out, err := w.push("origin")
				if err == nil || w.remoteHasMain() {
					t.Fatalf("control: an ungated push with the origin intact must be refused (err=%v):\n%s", err, out)
				}
				if !strings.Contains(out, "no review record") {
					t.Fatalf("control: refused for the wrong reason:\n%s", out)
				}

				// The case: the remote renamed. The profile cannot be found by the origin's name.
				gitRun(t, w.repo, "remote", "rename", "origin", "x")
				out, err = w.push("x")
				if err == nil {
					t.Errorf("the push went through the pre-push hook with no review record:\n%s", out)
				}
				if w.remoteHasMain() {
					t.Errorf("a commit with no review record reached the remote:\n%s", out)
				}
				if !strings.Contains(out, "cannot be known") {
					t.Errorf("the refusal must say the gate cannot be determined, got:\n%s", out)
				}
			})
		}
	}
}

// installPrePush writes a pre-push hook that runs the binary's check, as `leadkit init` does.
func installPrePush(t *testing.T, repo, bin string) {
	t.Helper()
	hook := filepath.Join(repo, ".git", "hooks", "pre-push")
	script := "#!/bin/sh\nexec " + bin + " gate check-push\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// push runs a real `git push` of main to the named remote with the sandboxed environment.
func (w namingWorld) push(remote string) (string, error) {
	cmd := exec.Command("git", "push", remote, "main")
	cmd.Dir = w.repo
	cmd.Env = w.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// remoteHasMain reports whether the bare remote has a main branch.
func (w namingWorld) remoteHasMain() bool {
	return exec.Command("git", "-C", w.bare, "rev-parse", "--verify", "-q", "refs/heads/main").Run() == nil
}
