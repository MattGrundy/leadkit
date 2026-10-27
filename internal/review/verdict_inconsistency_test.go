package review

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// The package's central invariant: a failure AFTER gate.Record has written the record is never
// silent, because by then the record and the ticket's location/frontmatter disagree. GateVerdict
// marks each such failure with loudInconsistency, once per routing branch (changes-needed, hand-off
// to the next layer, guard-failed, final routing).
//
// Mutant R19 (one per branch, ticket 331): replace `loudInconsistency(err)` with `err` at that
// branch. Each test below drives exactly one branch to fail and asserts the loud marker, that the
// cause is still reachable through it, that the record IS on disk (so the marker is telling the
// truth), and that the ticket did not move (so "reconcile" has something to reconcile).

// assertLoud is the shared claim: err is the INCONSISTENT STATE error and wraps cause-bearing text.
func assertLoud(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the post-Record routing failure was swallowed: want an error")
	}
	if !strings.Contains(err.Error(), "INCONSISTENT STATE") {
		t.Errorf("a failure after the record was written must say INCONSISTENT STATE, got %q", err)
	}
	if !strings.Contains(err.Error(), "the gate record was written") {
		t.Errorf("the error must tell the operator the record WAS written, got %q", err)
	}
	if errors.Unwrap(err) == nil {
		t.Errorf("the original cause must stay reachable through the wrapper, got %q", err)
	}
}

func assertRecordedButNotMoved(t *testing.T, g *gate.Gate, layer, verdict, ticket string) {
	t.Helper()
	rec, err := g.ReadRecord(verdictTestSHA)
	if err != nil {
		t.Fatalf("the marker claims the record was written, but there is none: %v", err)
	}
	if rec.Layers[layer].Verdict != verdict {
		t.Errorf("record %s = %q, want %q", layer, rec.Layers[layer].Verdict, verdict)
	}
	if _, err := os.Lstat(ticket); err != nil {
		t.Errorf("the routing failed, so the ticket must still be where it was: %v", err)
	}
}

// blockLane makes lane a regular file, so no ticket can be moved into queue/<lane>/<state>/ (the
// directory cannot be created under a file), on every platform and for root. queue.Move creates a
// missing destination directory, so merely leaving the lane absent does NOT make the move fail.
func blockLane(t *testing.T, rv *Reviewer, lane string) {
	t.Helper()
	p := filepath.Join(rv.QueueRoot, lane)
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not a lane"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Branch 1: changes-needed. The owner's lane is blocked, so the move fails after the
// record was written.
func TestGateVerdict_ChangesNeededRoutingFailureIsLoud(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	blockLane(t, rv, "dev-owner")
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner"}})
	assertLoud(t, err)
	assertRecordedButNotMoved(t, g, "code-review", "changes-needed", link)
}

// Branch 2: a ready that hands the ticket to the next layer's lane.
func TestGateVerdict_HandOffRoutingFailureIsLoud(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	blockLane(t, rv, "critique")
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	assertLoud(t, err)
	assertRecordedButNotMoved(t, g, "code-review", "ready", link)
}

// Branch 3: the last layer is recorded ready but the gate still does not pass, so the ticket is
// left in place with status pending; the status write fails. A layer the chain does not list is how
// a ready last verdict leaves the gate failing here: security is a valid layer, off the default
// chain, so its ready does not complete code-review and critique.
//
// The status write is made to fail by making the ticket file read-only, which root ignores.
func TestGateVerdict_GuardFailedFrontmatterFailureIsLoud(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	rv, g, link, _ := setupVerdictFixture(t)
	g.ReviewRoles = []string{"security"}
	canonical := filepath.Join(rv.TasksDir, "9-example.md")

	// Precondition, in the same fixture with the file writable: this verdict does take the
	// guard-failed branch, so the read-only run below fails in the branch it claims to.
	pre, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "sec-1", Layer: "security", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pre.GuardFailed {
		t.Fatalf("precondition: a ready security with code-review and critique unrecorded must leave the gate failing, got %+v", pre)
	}

	if err := os.Chmod(canonical, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(canonical, 0o644) })

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "sec-1", Layer: "security"})
	assertLoud(t, err)
	assertRecordedButNotMoved(t, g, "security", "ready", link)
	if !res.GuardFailed {
		t.Errorf("the failure is in the guard-failed branch: res.GuardFailed should be set, got %+v", res)
	}
}

// Branch 4: the last layer ready with the gate passing, routed to finalise. finalise is not live
// (its lane is blocked), so the ticket is filed under the recorder's own done lane, which is blocked too,
// and the move fails.
func TestGateVerdict_FinalRoutingFailureIsLoud(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}
	blockLane(t, rv, "finalise")
	blockLane(t, rv, "crit-1")

	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "crit-1"})
	assertLoud(t, err)
	assertRecordedButNotMoved(t, g, "critique", "ready", critLink)
}
