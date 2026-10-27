package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_TaskNote_ReachesATicketThatWasRoutedAway reproduces the observed loss with the real
// binary: code-review records its verdict (which routes the ticket to critique), then writes its
// note to the lane path it had held. A shell append there exits 0 and creates an orphan file;
// `task note` must put the note in the ticket instead.
func TestCLI_TaskNote_ReachesATicketThatWasRoutedAway(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	proj, err := filepath.EvalSymlinks(t.TempDir()) // macOS temp dirs sit behind /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"tasks", "queue/developer/pending", "queue/code-review/pending", "queue/code-review/in-progress", "queue/critique/pending"} {
		if err := os.MkdirAll(filepath.Join(proj, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = proj, sandboxedEnv(home, nil)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "git@example.com:o/r.git")
	git("config", "user.email", "a@b.c")
	git("config", "user.name", "n")
	if err := os.WriteFile(filepath.Join(proj, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-qm", "init")
	profile := filepath.Join(proj, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("project_id: r\nqueue_root: ./queue\nroster:\n  - type: developer\n    count: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir, c.Env = proj, sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile})
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("leadkit %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	run("task", "create", "--type", "developer", "--unparented", "--title", "x", "--assign", "developer")
	run("review", "send", "--task", "1-x", "--repo", proj)
	run("queue", "move", "-queue", filepath.Join(proj, "queue"), filepath.Join(proj, "queue/code-review/pending/1-x.md"), filepath.Join(proj, "queue/code-review/in-progress")+"/")
	lane := filepath.Join(proj, "queue/code-review/in-progress/1-x.md")
	run("gate", "verdict", "--ticket", lane, "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1")
	if _, err := os.Lstat(lane); !os.IsNotExist(err) {
		t.Fatalf("precondition: the verdict must have routed the ticket away from %s", lane)
	}

	out := run("task", "note", "--task", lane, "--by", "cr-1", "--text", "REVIEW NOTE: found nothing")
	if !strings.Contains(out, "noted:") {
		t.Errorf("no confirmation: %s", out)
	}
	ticket, _ := os.ReadFile(filepath.Join(proj, "tasks", "1-x.md"))
	if !strings.Contains(string(ticket), "REVIEW NOTE: found nothing") {
		t.Errorf("the note is not in the ticket:\n%s", ticket)
	}
	if _, err := os.Lstat(lane); !os.IsNotExist(err) {
		t.Error("`task note` left a stray file at the old lane path")
	}

	// And a ticket that does not exist is a loud refusal, not a new file.
	c := exec.Command(bin, "task", "note", "--task", filepath.Join(proj, "queue/code-review/in-progress/404-none.md"), "--by", "cr-1", "--text", "x")
	c.Dir, c.Env = proj, sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile})
	if out, err := c.CombinedOutput(); err == nil || !strings.Contains(string(out), "nothing was written anywhere") {
		t.Errorf("unknown ticket: want a loud exit, got err=%v\n%s", err, out)
	}
}
