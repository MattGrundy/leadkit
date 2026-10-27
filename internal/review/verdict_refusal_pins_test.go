package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// Ticket 331: survivors of the ticket-156 sweep (and of a wider scan of verdict.go made while
// working it) in GateVerdict's own validation, which runs before anything is written. Each test
// names the mutant it defends and was seen to fail under it. The mutants are described in terms of
// the code, because the sweep's own labels for them were not preserved.

const baseTestSHA = "cccccccccccccccccccccccccccccccccccccccc"

// baseResolvingGit resolves baseTestSHA^{commit} as well as the ticket's sha, so a --base can be
// genuinely resolvable.
type baseResolvingGit struct{ fakeVerdictGit }

func (b baseResolvingGit) RevParse(repo, ref string) (string, bool) {
	if ref == "base1^{commit}" {
		return baseTestSHA, true
	}
	return b.fakeVerdictGit.RevParse(repo, ref)
}

func assertNoRecord(t *testing.T, g *gate.Gate) {
	t.Helper()
	if _, err := g.ReadRecord(verdictTestSHA); err == nil {
		t.Error("a gate record was written by a verdict that should have been refused or only planned")
	}
}

// Mutant: delete the `repo == ""` refusal. The ticket then goes on with no repo, and the sha is
// resolved, the scope judged and the record written against the empty repo (here the stub resolves
// anything), so a ticket with no repo is recorded against nowhere instead of being refused.
func TestGateVerdict_RefusesATicketWithNoRepo(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"repo", ""}}); err != nil {
		t.Fatal(err)
	}
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	if err == nil || !strings.Contains(err.Error(), "has no repo in frontmatter") {
		t.Fatalf("err = %v, want a refusal naming the missing repo", err)
	}
	assertNoRecord(t, g)
}

// Mutant: delete the `opts.Verdict != "ready"` refusal of --base. A base qualifies a ready verdict
// only. Record refuses the same thing, but a DRY RUN never calls Record, so without GateVerdict's own
// check a dry run would approve a command the real run refuses, which defeats a dry run.
func TestGateVerdict_DryRunRefusesABaseOnAChangesNeeded(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	g.Git = baseResolvingGit{fakeVerdictGit{remote: "github.com/org/acmecorp-product"}}
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Base: "base1", DryRun: true, Roster: []string{"dev-owner"}})
	if err == nil || !strings.Contains(err.Error(), "qualifies a ready verdict only") {
		t.Fatalf("err = %v, want the --base-on-a-changes-needed refusal", err)
	}
	if !strings.HasPrefix(err.Error(), "gate-verdict: refuse") {
		t.Errorf("err = %q: the refusal must come from GateVerdict's own validation, not from a later layer", err)
	}
	assertNoRecord(t, g)
}

// Mutant: delete the `ResolveCommit(repo, opts.Base)` refusal. Same reasoning: a dry run never
// reaches Record's own check, so it would plan a record with a base that does not exist. The positive
// control below is what shows the refusal is about resolution and not about --base as such.
func TestGateVerdict_DryRunRefusesABaseThatDoesNotResolve(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	g.Git = baseResolvingGit{fakeVerdictGit{remote: "github.com/org/acmecorp-product"}}
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1", Base: "no-such-ref", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "does not resolve to a commit") {
		t.Fatalf("err = %v, want the unresolvable --base refusal", err)
	}
	if !strings.HasPrefix(err.Error(), "gate-verdict: refuse") {
		t.Errorf("err = %q: the refusal must come from GateVerdict's own validation, not from a later layer", err)
	}
	assertNoRecord(t, g)
}

func TestGateVerdict_AResolvableBaseIsRecordedOnTheLayer(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	g.Git = baseResolvingGit{fakeVerdictGit{remote: "github.com/org/acmecorp-product"}}
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1", Base: "base1"}); err != nil {
		t.Fatalf("a resolvable --base on a ready verdict: %v", err)
	}
	rec, err := g.ReadRecord(verdictTestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Layers["code-review"].Base; got != baseTestSHA {
		t.Errorf("layer base = %q, want the resolved %q", got, baseTestSHA)
	}
}

// Mutant: replace layer = chain[len(chain)-1] with chain[0] in the layer derivation. When the gate
// cannot JUDGE the commit (its scope is unresolved) but the record already has every layer ready,
// the layer to record is the last of the chain ("falls to the last"). The first-layer reading would
// send a critique reviewer's verdict to the code-review layer, which its role may not record.
func TestGateVerdict_WithEveryLayerReadyTheDerivedLayerIsTheLast(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	for _, r := range []gate.RecordOpts{
		{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo},
		{SHA: verdictTestSHA, Layer: "critique", Verdict: "ready", By: "crit-1", Repo: repo},
	} {
		if _, err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	// An explicit enforced_remotes list and an origin that cannot be read: Check allows, and says it
	// is unverifiable, so GateVerdict neither reports "already gated" nor "out of scope".
	g.ProjectRoot = ""
	g.EnforcedRemotes = []string{"some-other-repo"}
	g.Git = fakeVerdictGit{remote: ""}
	if ok, outcome, _ := g.CheckWithOutcome(verdictTestSHA, repo); !ok || outcome != gate.OutcomeUnverifiable {
		t.Fatalf("precondition: want an unverifiable allow, got ok=%v outcome=%v", ok, outcome)
	}

	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "crit-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if res.Layer != "critique" {
		t.Errorf("derived layer = %q, want critique (the last of the chain, every layer being ready)", res.Layer)
	}
}

// Mutant: change the changes-needed-needs-an-owner refusal to never fire. The ticket then falls
// to the owner reconciliation with "", which also refuses, but with "does not reconcile to a known
// role", telling the operator to "set a canonical owner" when the ticket has no owner AND no type to
// default one from. The existing no-owner test asserts only that SOME error comes back.
func TestGateVerdict_ChangesNeededWithNoOwnerNorTypeSaysSo(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", ""}}); err != nil {
		t.Fatal(err)
	}
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1"})
	if err == nil || !strings.Contains(err.Error(), "changes-needed requires an owner") {
		t.Fatalf("err = %v, want the missing-owner refusal that names owner: and type:", err)
	}
	assertNoRecord(t, g)
}

// Mutant: change the default By "claude" to anything else. An empty By is recorded as "claude", which
// the gate's role rules treat as a role of its own (so an unattributed verdict is not a cr or a crit).
func TestGateVerdict_AnEmptyByIsRecordedAsClaude(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready"}); err != nil {
		t.Fatal(err)
	}
	rec, err := g.ReadRecord(verdictTestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Layers["code-review"].By; got != "claude" {
		t.Errorf("By = %q, want the default %q", got, "claude")
	}
}
