package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_TwoOwnersOneTicket drives the incident through the real binary: two reviewers on one lane, one
// ticket. The second must be refused the claim with the holder named, must not be woken by the first's
// in-progress ticket, and the first still is on a restart.
func TestCLI_TwoOwnersOneTicket(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(root, "queue")
	lane := filepath.Join(queue, "code-review")
	for _, d := range []string{"tasks", "queue/code-review/pending", "queue/code-review/in-progress", "queue/code-review/done"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "9-x.md"), []byte("---\ntitle: t\nstatus: in_review\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(lane, "pending", "9-x.md")
	if err := os.Symlink("../../../tasks/9-x.md", pending); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(lane, "in-progress", "9-x.md")

	if out, code := runQueueCLI(t, bin, home, "queue", "claim", "-queue", queue, pending); code != 2 || !strings.Contains(out, "usage") {
		t.Errorf("a claim with no -by must be a usage error (exit %d):\n%s", code, out)
	}
	if out, code := runQueueCLI(t, bin, home, "queue", "claim", "-queue", queue, "-by", "cr-owner-4", pending); code != 0 || !strings.Contains(out, `claimed by "cr-owner-4"`) {
		t.Fatalf("first claim (exit %d):\n%s", code, out)
	}
	if _, err := os.Lstat(entry); err != nil {
		t.Fatalf("the claim did not move the ticket into in-progress: %v", err)
	}

	out, code := runQueueCLI(t, bin, home, "queue", "claim", "-queue", queue, "-by", "cr-owner-5", entry)
	if code != 1 || !strings.Contains(out, "cr-owner-4") || !strings.Contains(out, "already") {
		t.Errorf("the second owner's claim must be refused, naming the holder (exit %d):\n%s", code, out)
	}

	out, code = runQueueCLI(t, bin, home, "queue", "wait", "-by", "cr-owner-5", lane, "1", "1")
	if code != 0 || !strings.Contains(out, "TIMEOUT") {
		t.Errorf("the second owner must stay parked while the first holds the ticket (exit %d):\n%s", code, out)
	}
	out, code = runQueueCLI(t, bin, home, "queue", "wait", "-by", "cr-owner-4", lane, "1", "1")
	if code != 0 || !strings.Contains(out, "ORPHAN-INPROGRESS: 9-x.md") {
		t.Errorf("the holder must still be woken for its own claim (exit %d):\n%s", code, out)
	}
	// An existing caller that passes no -by and no flags is unchanged: the positional form still parses.
	out, code = runQueueCLI(t, bin, home, "queue", "wait", lane, "1", "1")
	if code != 0 || !strings.Contains(out, "ORPHAN-INPROGRESS: 9-x.md") {
		t.Errorf("the legacy positional form must keep working (exit %d):\n%s", code, out)
	}

	// Leaving in-progress ends the claim, so the next stay starts unclaimed.
	if out, code := runQueueCLI(t, bin, home, "queue", "move", "-queue", queue, entry, filepath.Join(lane, "done")+"/"); code != 0 {
		t.Fatalf("move to done (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(lane, "in-progress", ".claims", "9-x.md.json")); err == nil {
		t.Error("the claim file outlived the ticket leaving in-progress")
	}
}

// claimAs runs `queue claim` with CLAUDE_AGENT_NAME set to identity ("" leaves it genuinely unset).
func claimAs(t *testing.T, bin, home, identity string, args ...string) (string, int) {
	t.Helper()
	var extra []string
	if identity != "" {
		extra = []string{"CLAUDE_AGENT_NAME=" + identity}
	}
	cmd := exec.Command(bin, append([]string{"queue", "claim"}, args...)...)
	cmd.Dir = home
	var env []string
	for _, kv := range sandboxedEnv(home, extra) {
		if identity == "" && strings.HasPrefix(kv, "CLAUDE_AGENT_NAME=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec: %v", err)
	return "", -1
}

// The claim is filed under the same name the gate record and verdict will carry: with an identity the
// identity IS the claimant (-by optional, a different -by refused with nothing claimed); without one a
// reviewer-role -by is accepted but flagged, because that is how claim and record names drift apart.
func TestCLI_QueueClaim_IsBoundToTheSessionsIdentity(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(root, "queue")
	lane := filepath.Join(queue, "code-review")
	for _, d := range []string{"tasks", "queue/code-review/pending", "queue/code-review/in-progress"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"1-a.md", "2-b.md"} {
		if err := os.WriteFile(filepath.Join(root, "tasks", n), []byte("---\ntitle: t\nstatus: in_review\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../tasks/"+n, filepath.Join(lane, "pending", n)); err != nil {
			t.Fatal(err)
		}
	}
	a, b := filepath.Join(lane, "pending", "1-a.md"), filepath.Join(lane, "pending", "2-b.md")

	if out, code := claimAs(t, bin, home, "cr-1", "-queue", queue, "-by", "crit-1", a); code != 2 || !strings.Contains(out, "CLAUDE_AGENT_NAME") {
		t.Errorf("a -by that is not the session's identity must be refused (exit %d):\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(lane, "in-progress", "1-a.md")); err == nil {
		t.Error("a refused claim moved the ticket")
	}
	if out, code := claimAs(t, bin, home, "cr-1", "-queue", queue, a); code != 0 || !strings.Contains(out, `claimed by "cr-1"`) || strings.Contains(out, "self-asserted") {
		t.Errorf("-by omitted must claim as the identity, silently (exit %d):\n%s", code, out)
	}
	if out, code := claimAs(t, bin, home, "cr-1", "-queue", queue, "-by", "cr-1", filepath.Join(lane, "in-progress", "1-a.md")); code != 0 {
		t.Errorf("repeating the identity as -by is fine (exit %d):\n%s", code, out)
	}
	if out, code := claimAs(t, bin, home, "", "-queue", queue, b); code != 2 || !strings.Contains(out, "usage") {
		t.Errorf("no identity and no -by is a usage error (exit %d):\n%s", code, out)
	}
	out, code := claimAs(t, bin, home, "", "-queue", queue, "-by", "cr-2", b)
	if code != 0 || !strings.Contains(out, `claimed by "cr-2"`) || !strings.Contains(out, unboundNote) {
		t.Errorf("an unbound reviewer-role claim must succeed with a warning (exit %d):\n%s", code, out)
	}
	// The note is for a ticket's FIRST claim: renewing it (in-progress, same holder) is silent, and a call
	// refused for a misplaced flag says nothing before refusing.
	if out, code := claimAs(t, bin, home, "", "-queue", queue, "-by", "cr-2", filepath.Join(lane, "in-progress", "2-b.md")); code != 0 || strings.Contains(out, unboundNote) {
		t.Errorf("a renewal must not repeat the warning (exit %d):\n%s", code, out)
	}
	if out, code := claimAs(t, bin, home, "", "-queue", queue, "-by", "cr-2", "--", "-oops"); code != 2 || !strings.Contains(out, "misplaced flag") || strings.Contains(out, unboundNote) {
		t.Errorf("a refused call must not warn (exit %d):\n%s", code, out)
	}
}
