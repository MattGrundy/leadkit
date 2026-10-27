package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/queue"
)

// claimFixture leaves the verdict fixture's ticket claimed by cr-owner-4 in code-review's in-progress.
func claimFixture(t *testing.T) (rv *Reviewer, g *gate.Gate, entry string) {
	t.Helper()
	rv, g, link, _ := setupVerdictFixture(t)
	if _, err := queue.ClaimEntry(rv.QueueRoot, link, "cr-owner-4", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	return rv, g, filepath.Join(rv.QueueRoot, "code-review", "in-progress", "9-example.md")
}

func TestGateVerdict_RefusesAVerdictOnATicketAPeerHasClaimedBeforeAnythingIsRecorded(t *testing.T) {
	rv, g, entry := claimFixture(t)
	ticket, err := os.ReadFile(filepath.Join(rv.TasksDir, "9-example.md"))
	if err != nil {
		t.Fatal(err)
	}

	for _, dry := range []bool{false, true} {
		_, err = rv.GateVerdict(g, VerdictOpts{Ticket: entry, Verdict: "ready", By: "cr-owner-5", DryRun: dry})
		if err == nil {
			t.Fatalf("dry=%v: cr-owner-5 got a verdict through on cr-owner-4's claimed ticket", dry)
		}
		for _, want := range []string{"cr-owner-4", "claimed", "cr-owner-5"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("dry=%v: refusal lacks %q: %v", dry, want, err)
			}
		}
	}
	if _, rerr := g.ReadRecord(verdictTestSHA); rerr == nil {
		t.Error("a refused verdict wrote a gate record")
	}
	if after, _ := os.ReadFile(filepath.Join(rv.TasksDir, "9-example.md")); string(after) != string(ticket) {
		t.Error("a refused verdict edited the ticket")
	}
	if _, err := os.Lstat(entry); err != nil {
		t.Error("a refused verdict moved the ticket")
	}
}

func TestGateVerdict_TheHolderRecordsAndTheClaimEndsWithTheTicketsStay(t *testing.T) {
	rv, g, entry := claimFixture(t)
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: entry, Verdict: "ready", By: "cr-owner-4"}); err != nil {
		t.Fatalf("the holder's own verdict: %v", err)
	}
	if _, ok := queue.ReadClaim(entry); ok {
		t.Error("the claim outlived the ticket leaving in-progress")
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")); err != nil {
		t.Errorf("the ticket was not routed on: %v", err)
	}
}

func TestGateVerdict_ALapsedClaimDoesNotBlockAPeer(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if _, err := queue.ClaimEntry(rv.QueueRoot, link, "cr-owner-4", time.Now().Add(-5*time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(rv.QueueRoot, "code-review", "in-progress", "9-example.md")
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: entry, Verdict: "ready", By: "cr-owner-5"}); err != nil {
		t.Fatalf("a dead holder's claim must not block the lane: %v", err)
	}
}

func TestGateVerdict_AnUnclaimedTicketStillTakesAnyReviewer(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-owner-5"}); err != nil {
		t.Fatalf("a single owner on an unclaimed ticket is unaffected: %v", err)
	}
}

func TestSendToReview_RefusesToResendATicketAReviewerHasClaimed(t *testing.T) {
	rv := setupReviewFixture(t)
	base := "1-x.md"
	writeTicket(t, rv, base, sampleTicket)
	repo := initGitRepo(t, "clean commit")
	if _, err := rv.SendToReview(base, repo, "", false); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(rv.QueueRoot, "code-review", "pending", base)
	if _, err := queue.ClaimEntry(rv.QueueRoot, pending, "cr-owner-4", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(rv.TasksDir, base))

	for _, dry := range []bool{true, false} {
		_, err := rv.SendToReview(base, repo, "", dry)
		if err == nil || !strings.Contains(err.Error(), "cr-owner-4") || !strings.Contains(err.Error(), "queue unlink") {
			t.Fatalf("dry=%v: re-sending a claimed ticket: %v", dry, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(rv.TasksDir, base)); string(after) != string(before) {
		t.Error("a refused send edited the ticket")
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "code-review", "in-progress", base)); err != nil {
		t.Error("a refused send removed the reviewer's entry")
	}

	// Once the claim lapses the reviewer is presumed gone and the re-send goes through.
	entry := filepath.Join(rv.QueueRoot, "code-review", "in-progress", base)
	if _, err := queue.ClaimEntry(rv.QueueRoot, entry, "cr-owner-4", time.Now().Add(-5*time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.SendToReview(base, repo, "", false); err != nil {
		t.Errorf("a lapsed claim blocked the re-send: %v", err)
	}
}
