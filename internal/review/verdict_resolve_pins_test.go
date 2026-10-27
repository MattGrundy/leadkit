package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// Ticket 331 (found by the ticket-156 mutation sweep): the queue-root containment check and the
// canonical tasks-file check in resolveVerdictTicket decide WHICH ticket a verdict is recorded
// against, before anything is written. TestGateVerdict_RefusesTicketOutsideQueueRoot only refuses a
// ticket whose directory is nowhere near the queue, which any version of the check refuses.

// assertRefusedBeforeAnyRecord is the shared claim for a ticket the resolver must refuse: the named
// refusal, and no record on disk. A mutant that lets the ticket through gets as far as gate.Record
// and is only stopped afterwards by queue.Move, with the record already written and the ticket
// not moved, which is the record/location disagreement the package exists to prevent.
func assertRefusedBeforeAnyRecord(t *testing.T, g *gate.Gate, err error, wantInErr string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), wantInErr) {
		t.Errorf("refusal = %q, want it to say %q (a later check refusing it for another reason is not this check)", err, wantInErr)
	}
	if _, rerr := g.ReadRecord(verdictTestSHA); rerr == nil {
		t.Error("a gate record was written for a ticket the resolver should have refused")
	}
}

// Mutant R14: in resolveVerdictTicket, compare the ticket's state dir against the queue root with
// strings.HasPrefix(parentReal, queueRootReal) instead of queueRootReal+separator. A SIBLING
// directory whose name merely starts with the queue root's ("queue2" beside "queue") then passes as
// inside it.
func TestGateVerdict_RefusesATicketInASiblingOfTheQueueRoot(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	// queue2/code-review/pending/ beside queue/: same tickets, the lookalike root.
	sibling := rv.QueueRoot + "2"
	siblingPending := filepath.Join(sibling, "code-review", "pending")
	if err := os.MkdirAll(siblingPending, 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	siblingLink := filepath.Join(siblingPending, "9-example.md")
	// The fixture's link is relative (../../../tasks/...), and the sibling is the same depth.
	if err := os.Symlink(target, siblingLink); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(siblingLink); err != nil {
		t.Fatalf("precondition: the sibling link must resolve to the real ticket: %v", err)
	}

	_, err = rv.GateVerdict(g, VerdictOpts{Ticket: siblingLink, Verdict: "ready", By: "cr-1"})
	assertRefusedBeforeAnyRecord(t, g, err, "not inside the queue root")
}

// Mutant R14 (second form): a ticket sitting directly in the queue root, not in a lane's state dir.
// Its parent IS the queue root, which "inside" must not include; the separator in the prefix is what
// excludes it.
func TestGateVerdict_RefusesATicketDirectlyInTheQueueRoot(t *testing.T) {
	rv, g, _, _ := setupVerdictFixture(t)
	direct := filepath.Join(rv.QueueRoot, "9-example.md")
	// One directory shallower than the lane's pending/ the target was written for.
	if err := os.Symlink(filepath.Join("..", "tasks", "9-example.md"), direct); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(direct); err != nil {
		t.Fatalf("precondition: the link must resolve to the real ticket: %v", err)
	}

	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: direct, Verdict: "ready", By: "cr-1"})
	assertRefusedBeforeAnyRecord(t, g, err, "not inside the queue root")
}

// Mutant R14d: delete the canonical tasks-file check (the os.Stat of TasksDir/<base>). The ticket is
// a real file inside the queue root, so every earlier check passes, but there is no tasks/<base> for
// the verdict to reconcile. Without the check the ticket is still refused, but by the frontmatter
// read, whose message does not say that the ticket is not under the tasks dir; the operator is told
// the wrong thing about what is broken.
func TestGateVerdict_RefusesATicketWithNoCanonicalTasksFile(t *testing.T) {
	rv, g, _, _ := setupVerdictFixture(t)
	orphan := filepath.Join(rv.QueueRoot, "code-review", "pending", "77-orphan.md")
	if err := os.WriteFile(orphan, []byte("---\nrepo: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: orphan, Verdict: "ready", By: "cr-1"})
	assertRefusedBeforeAnyRecord(t, g, err, "is not under the tasks dir")
}
