package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runQueueCLI execs the binary in the standard sandbox (HOME pinned, no inherited profile) from
// the HOME dir, so nothing depends on the cwd or touches the real queue.
func runQueueCLI(t *testing.T, bin, home string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = home, sandboxedEnv(home, nil)
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

// TestCLI_QueueStrandedTicket runs the incident through the real binary: a move to `active/` is
// refused with the valid states named, a ticket stranded there anyway is reported by sweep (which
// said "0 stalled, 0 with broken links"), and `queue unlink` clears the duplicate link without a
// raw rm and without touching the legitimate one.
func TestCLI_QueueStrandedTicket(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(root, "queue")
	for _, d := range []string{"tasks", "queue/developer/pending", "queue/developer/in-progress", "queue/developer/done"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "77-x.md"), []byte("---\ntitle: t\nstatus: pending\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(queue, "developer", "pending", "77-x.md")
	if err := os.Symlink("../../../tasks/77-x.md", pending); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		t.Helper()
		return runQueueCLI(t, bin, home, args...)
	}

	out, code := run("queue", "move", "-queue", queue, pending, filepath.Join(queue, "developer", "active")+"/")
	if code == 0 {
		t.Fatalf("a move to active/ must be refused, got exit 0:\n%s", out)
	}
	for _, want := range []string{`"active" is not a queue state`, "pending, in-progress, done"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Lstat(filepath.Join(queue, "developer", "active")); !os.IsNotExist(err) {
		t.Error("the refused move created active/")
	}

	// The stray as it exists in the wild (made before the move was refused).
	stray := filepath.Join(queue, "developer", "active", "77-x.md")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../tasks/77-x.md", stray); err != nil {
		t.Fatal(err)
	}

	// Claim the legitimate ticket, so the lane is NOT stalled and the stray is the ONLY finding:
	// otherwise -strict exits 1 for the stall whether or not the stranded ticket is counted.
	inProgress := filepath.Join(queue, "developer", "in-progress", "77-x.md")
	if out, code := run("queue", "move", "-queue", queue, pending, filepath.Join(queue, "developer", "in-progress")+"/"); code != 0 {
		t.Fatalf("claiming the ticket (exit %d):\n%s", code, out)
	}
	pending = inProgress

	out, code = run("queue", "sweep", "-queue", queue, "-strict")
	if code == 0 || !strings.Contains(out, "STRAY   developer/active") || !strings.Contains(out, "0 stalled, 0 with broken links, 1 stranded") {
		t.Errorf("sweep must report the stranded ticket and fail under -strict (exit %d):\n%s", code, out)
	}

	out, code = run("queue", "unlink", "-queue", queue, stray)
	if code != 0 || !strings.Contains(out, "WOULD remove") {
		t.Errorf("dry-run unlink (exit %d):\n%s", code, out)
	}
	if _, err := os.Lstat(stray); err != nil {
		t.Fatal("the dry run removed the link")
	}
	if out, code = run("queue", "unlink", "-queue", queue, "-apply", stray); code != 0 {
		t.Fatalf("unlink -apply (exit %d):\n%s", code, out)
	}
	if _, err := os.Lstat(stray); !os.IsNotExist(err) {
		t.Error("the stray link survived -apply")
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Error("the legitimate link was removed")
	}
	// Now the pending link is the ticket's only one, and removing it must be refused.
	if out, code = run("queue", "unlink", "-queue", queue, "-apply", pending); code == 0 || !strings.Contains(out, "ONLY link") {
		t.Errorf("unlinking a ticket's only link must be refused (exit %d):\n%s", code, out)
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Error("the refused unlink removed the only link anyway")
	}
	if out, code = run("queue", "sweep", "-queue", queue, "-strict"); code != 0 || !strings.Contains(out, "0 stranded") {
		t.Errorf("after the cleanup the sweep must read 0 stranded and exit 0 under -strict, though the emptied active/ is still there (exit %d):\n%s", code, out)
	}
}
