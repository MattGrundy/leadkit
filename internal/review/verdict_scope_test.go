package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// "already gated" is a claim about the commit. A gate that did not JUDGE it (out of scope, or a scope
// it could not read because the remote is not named origin) is not saying the commit is reviewed, and
// a reviewer who asked to record a verdict must get one recorded, not a success that discarded it.
func TestGateVerdict_AnUnjudgedCommitIsNotReportedAsAlreadyGated(t *testing.T) {
	for name, tweak := range map[string]func(g *gate.Gate){
		"the origin cannot be read": func(g *gate.Gate) { g.Git = fakeVerdictGit{remote: ""} },
	} {
		t.Run(name, func(t *testing.T) {
			rv, g, link, _ := setupVerdictFixture(t)
			tweak(g)
			res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
			if err != nil {
				t.Fatal(err)
			}
			if res.NoOp {
				t.Fatalf("a verdict nobody judged was discarded as 'already gated': %+v", res)
			}
			rec, rerr := g.ReadRecord(verdictTestSHA)
			if rerr != nil || rec.Layers["code-review"].Verdict != "ready" {
				t.Fatalf("the verdict was not recorded: %v %+v", rerr, rec)
			}
			if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")); err != nil {
				t.Errorf("the ticket was not routed on: %v", err)
			}
		})
	}
}

// ...while a commit that IS gated stays a NoOp, so the fix does not turn every verdict into a re-record.
func TestGateVerdict_AGatedCommitIsStillAlreadyGated(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	for _, r := range []gate.RecordOpts{
		{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo},
		{SHA: verdictTestSHA, Layer: "critique", Verdict: "ready", By: "crit-1", Repo: repo},
	} {
		if _, err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-2"})
	if err != nil || !res.NoOp {
		t.Fatalf("a fully gated commit must stay a NoOp: %+v %v", res, err)
	}
}

// A repo that is resolved and not this project's is a different case: nothing to record, and the message
// must not claim the commit is gated.
func TestGateVerdict_AnOutOfScopeRepoIsReportedAsNotRecordedNotAsGated(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	g.ProjectRoot = ""
	g.EnforcedRemotes = []string{"elsewhere"}
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	if err != nil || !res.NoOp {
		t.Fatalf("got %+v %v", res, err)
	}
	if !strings.Contains(res.Message, "NOT recorded") || strings.Contains(res.Message, "already gated") {
		t.Errorf("message = %q", res.Message)
	}
}
