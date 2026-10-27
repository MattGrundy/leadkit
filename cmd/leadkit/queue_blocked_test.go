package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_BlockedTicket_ClaimRefusedAndTheRefusalsRemedyWorks drives the incident through the real
// binary: a ticket marked blocked sits in pending, `queue move` refuses to claim it, and the verb
// the refusal names as the way out actually exists and works. (queue-guard has shipped refusals
// that name scripts which do not exist; this pins that the one here does.)
func TestCLI_BlockedTicket_ClaimRefusedAndTheRefusalsRemedyWorks(t *testing.T) {
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
	ticket := filepath.Join(root, "tasks", "87-x.md")
	if err := os.WriteFile(ticket, []byte("---\ntitle: t\nstatus: blocked\nblocked_on: \"owner decision\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(queue, "developer", "pending", "87-x.md")
	if err := os.Symlink("../../../tasks/87-x.md", pending); err != nil {
		t.Fatal(err)
	}

	out, code := runQueueCLI(t, bin, home, "queue", "wait", filepath.Join(queue, "developer"), "1", "1")
	if code != 0 || !strings.Contains(out, "TIMEOUT") {
		t.Errorf("a lane holding only a blocked ticket must read as idle (exit %d):\n%s", code, out)
	}

	// It is not stalled, so it does not fail -strict, but it must be REPORTED with its blocked_on.
	out, code = runQueueCLI(t, bin, home, "queue", "sweep", "-queue", queue, "-strict")
	if code != 0 || !strings.Contains(out, "BLOCKED developer — 87-x.md (blocked_on: owner decision)") || !strings.Contains(out, "0 stalled") || !strings.Contains(out, "1 blocked (reported, not stalled)") {
		t.Errorf("sweep must report the blocked ticket and still exit 0 under -strict (exit %d):\n%s", code, out)
	}

	out, code = runQueueCLI(t, bin, home, "queue", "move", "-queue", queue, pending, filepath.Join(queue, "developer", "in-progress")+"/")
	if code == 0 {
		t.Fatalf("claiming a blocked ticket must be refused:\n%s", out)
	}
	for _, want := range []string{"status: blocked", "blocked_on: owner decision", "queue unlink"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Fatal("the refused claim moved the ticket")
	}

	// The remedy the refusal names: take it out of the lane. It is the ticket's only link, hence -allow-last.
	out, code = runQueueCLI(t, bin, home, "queue", "unlink", "-queue", queue, "-allow-last", "-apply", pending)
	if code != 0 {
		t.Fatalf("the remedy the refusal names must work (exit %d):\n%s", code, out)
	}
	if _, err := os.Lstat(pending); !os.IsNotExist(err) {
		t.Error("the link is still in the lane")
	}
	if _, err := os.Stat(ticket); err != nil {
		t.Error("parking the ticket deleted it")
	}
}

// A ticket in a terminal status left in pending is reported, not stalled, and the summary line says so:
// the line is what an operator reads and a script greps, so the finished clause is part of the contract
// the way the blocked clause is.
func TestCLI_FinishedTicket_IsReportedNotStalledAndTheSummaryCountsIt(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(root, "tasks", "88-y.md"), []byte("---\ntitle: t\nstatus: done\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../tasks/88-y.md", filepath.Join(queue, "developer", "pending", "88-y.md")); err != nil {
		t.Fatal(err)
	}
	out, code := runQueueCLI(t, bin, home, "queue", "sweep", "-queue", queue, "-strict")
	if code != 0 || !strings.Contains(out, "FINISHED developer") || !strings.Contains(out, "88-y.md (status: done)") || !strings.Contains(out, "0 stalled") || !strings.Contains(out, "1 finished (reported, not stalled)") {
		t.Errorf("sweep must report the finished ticket, count it in the summary and exit 0 under -strict (exit %d):\n%s", code, out)
	}
}
