package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 228. A profile governs the project but the origin it is scoped by cannot be read.

func originUnreadableGate(t *testing.T) *Gate {
	t.Helper()
	g, _ := newTestGate(t, "") // no readable origin, ProjectRoot set: a profile governs
	t.Setenv("REVIEW_GATE_BYPASS", "")
	return g
}

// The reason names the cause and the fix, and does not call the repo "out of scope", which is a statement
// about policy that this is not.
func TestOriginUnreadable_ReasonNamesCauseAndFixNotPolicy(t *testing.T) {
	g := originUnreadableGate(t)
	ok, outcome, reason := g.CheckWithOutcome(testSHA, "")
	if !ok || outcome != OutcomeUnverifiable {
		t.Fatalf("advisory shape: ok=%v outcome=%v, want allow with OutcomeUnverifiable", ok, outcome)
	}
	if !OriginUnreadable(reason) {
		t.Errorf("OriginUnreadable must recognise the reason %q", reason)
	}
	if strings.Contains(reason, "out of scope") {
		t.Errorf("an unreadable origin is not an out-of-scope finding: %q", reason)
	}
	if !strings.Contains(OriginUnreadableFix, "git remote rename <name> origin") {
		t.Errorf("the fix must name the rename: %q", OriginUnreadableFix)
	}
}

// The legacy unresolved case (an explicit list to match and an origin that could not be read) keeps its
// text: only the unreadable-origin case has a reason of its own.
func TestOriginUnreadable_TheExplicitListCaseKeepsItsLegacyReason(t *testing.T) {
	g, _ := newTestGate(t, "")
	g.EnforcedRemotes = []string{"github.com/acme/other"}
	_, outcome, reason := g.CheckWithOutcome(testSHA, "")
	if outcome != OutcomeUnverifiable || reason != "out of scope — allow" || OriginUnreadable(reason) {
		t.Errorf("explicit-list unresolved: outcome=%v reason=%q, want OutcomeUnverifiable with the legacy text", outcome, reason)
	}
}

// The escape hatch the refusal names lifts it, in every mode, from both places the gate reads it.
func TestOriginUnreadable_BypassLiftsIt(t *testing.T) {
	g := originUnreadableGate(t)
	g.RefuseUnverifiable = true // enforce
	if ok, _, _ := g.CheckWithOutcome(testSHA, ""); ok {
		t.Fatal("control: in enforce mode the unreadable origin is refused")
	}

	t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
	ok, outcome, reason := g.CheckWithOutcome(testSHA, "")
	if !ok || outcome != OutcomeGated || !strings.Contains(reason, "BYPASS") {
		t.Errorf("env bypass: ok=%v outcome=%v reason=%q, want a logged BYPASS allow", ok, outcome, reason)
	}
	t.Setenv("REVIEW_GATE_BYPASS", "")

	if err := os.WriteFile(filepath.Join(g.RecordsDir, "BYPASS"), []byte("emergency"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, outcome, reason = g.CheckWithOutcome(testSHA, "")
	if !ok || outcome != OutcomeGated || !strings.Contains(reason, "BYPASS file") {
		t.Errorf("file bypass: ok=%v outcome=%v reason=%q, want a logged BYPASS allow", ok, outcome, reason)
	}
}

// RefuseUnverifiable turns exactly the Unverifiable outcome into a refusal: Gated and OutOfScope stay allowed,
// Ungated stays refused, and the outcome is unchanged for the callers that read it.
func TestRefuseUnverifiable_OnlyRefusesTheUnverifiableOutcome(t *testing.T) {
	g := originUnreadableGate(t)

	ok, outcome, _ := g.CheckWithOutcome(testSHA, "")
	if !ok || outcome != OutcomeUnverifiable {
		t.Fatalf("flag off: ok=%v outcome=%v, want the legacy fail-open", ok, outcome)
	}
	g.RefuseUnverifiable = true
	ok, outcome, reason := g.CheckWithOutcome(testSHA, "")
	if ok || outcome != OutcomeUnverifiable {
		t.Errorf("flag on: ok=%v outcome=%v, want a refusal that still reports OutcomeUnverifiable", ok, outcome)
	}
	if !OriginUnreadable(reason) || !strings.Contains(reason, "refused") || strings.HasSuffix(reason, "— allow") {
		t.Errorf("the refusal must keep the cause and say it is a refusal, not an allow: %q", reason)
	}
	if ok, _ := g.Check(testSHA, ""); ok {
		t.Error("Check (the ok-bool callers read) must carry the refusal")
	}

	out, _ := newTestGate(t, "github.com/acme/elsewhere")
	out.ProjectRoot = "" // unprofiled: genuinely not governed
	out.RefuseUnverifiable = true
	if ok, outcome, _ := out.CheckWithOutcome(testSHA, ""); !ok || outcome != OutcomeOutOfScope {
		t.Errorf("out of scope must stay allowed under the flag: ok=%v outcome=%v", ok, outcome)
	}
	in, _ := newTestGate(t, "github.com/acme/mine")
	in.RefuseUnverifiable = true
	if ok, outcome, _ := in.CheckWithOutcome(testSHA, ""); ok || outcome != OutcomeUngated {
		t.Errorf("an in-scope unrecorded sha stays ungated: ok=%v outcome=%v", ok, outcome)
	}
}

// CheckPush (the pre-push hook's reader of the ok-bool) refuses the unreadable origin only under the flag.
func TestOriginUnreadable_CheckPushFollowsTheFlag(t *testing.T) {
	g := originUnreadableGate(t)
	line := "refs/heads/main " + testSHA + " refs/heads/main 0000000000000000000000000000000000000000\n"
	if ok, err := g.CheckPush(strings.NewReader(line), ""); err != nil || !ok {
		t.Fatalf("flag off: ok=%v err=%v, want allow", ok, err)
	}
	g.RefuseUnverifiable = true
	if ok, err := g.CheckPush(strings.NewReader(line), ""); err != nil || ok {
		t.Errorf("flag on: ok=%v err=%v, want refusal", ok, err)
	}
}
