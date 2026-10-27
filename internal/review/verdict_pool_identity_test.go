package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/queue"
)

// Pool consumers that each carry ONE distinct name through claim, record and verdict walk a ticket
// through the whole chain: cr-1 claims and verdicts code-review, crit-1 claims and verdicts critique.
// The independence guard is what keeps them apart, so it must not be what stops them.
func TestGateVerdict_DistinctPoolConsumersAdvanceTheChain(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	now, ttl := time.Now(), time.Hour

	if _, err := queue.ClaimEntry(rv.QueueRoot, link, "cr-1", now, ttl); err != nil {
		t.Fatal(err)
	}
	crEntry := filepath.Join(rv.QueueRoot, "code-review", "in-progress", "9-example.md")
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: crEntry, Verdict: "ready", By: "cr-1", ByBound: true})
	if err != nil {
		t.Fatalf("cr-1 verdict on its own claim: %v", err)
	}
	if res.Layer != "code-review" || res.NewStage != "critique" {
		t.Fatalf("cr-1 verdict: %+v", res)
	}

	critPending := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if _, err := queue.ClaimEntry(rv.QueueRoot, critPending, "crit-1", now, ttl); err != nil {
		t.Fatal(err)
	}
	critEntry := filepath.Join(rv.QueueRoot, "critique", "in-progress", "9-example.md")

	// The same consumer cannot satisfy both layers, even holding no claim on the second.
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: critEntry, Verdict: "ready", By: "cr-1", ByBound: true}); err == nil {
		t.Fatal("cr-1 recorded critique after recording code-review: one reviewer satisfied both layers")
	}
	// A caller with a different name than the claimant (the lead's shell) is refused too.
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: critEntry, Verdict: "ready", By: "claude"}); err == nil || !strings.Contains(err.Error(), "crit-1") {
		t.Fatalf("a verdict from a name other than the claimant must be refused naming the holder: %v", err)
	}

	res, err = rv.GateVerdict(g, VerdictOpts{Ticket: critEntry, Verdict: "ready", By: "crit-1", ByBound: true})
	if err != nil {
		t.Fatalf("crit-1 verdict on its own claim: %v", err)
	}
	if res.Layer != "critique" || res.NewStage != "finalise" {
		t.Fatalf("crit-1 verdict: %+v", res)
	}
	rec, err := g.ReadRecord(verdictTestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Layers["code-review"].By != "cr-1" || rec.Layers["critique"].By != "crit-1" {
		t.Errorf("record names: %+v", rec.Layers)
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "finalise", "pending", "9-example.md")); err != nil {
		t.Errorf("ticket not routed to finalise: %v", err)
	}
}
