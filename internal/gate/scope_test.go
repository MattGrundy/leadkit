package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- gate scope: a project's OWN origin (ProjectRoot's origin) is ALWAYS gated,
// whatever enforced_remotes holds; the list ADDS other repos by substring. ---

// TestInScope_ExplicitList_SubstringMatch is the explicit-list path (unchanged): a
// project that declares its own repo in enforced_remotes comes into scope, and a
// verdict then becomes consequential (no record blocks; both layers ready passes).
func TestInScope_ExplicitList_SubstringMatch(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/another-project")
	g.EnforcedRemotes = []string{"another-project"}
	g.ProjectRoot = "" // no own origin: only the LIST can put this repo in scope

	if g.inScope("") != true {
		t.Fatal("want another-project in scope once enforced_remotes names it")
	}

	// With no record, Check must BLOCK (in scope, nothing recorded).
	if ok, reason := g.Check(testSHA, ""); ok {
		t.Errorf("want BLOCK for an in-scope repo with no record, got allow (%s)", reason)
	}

	// Recording both layers must then let it pass.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatalf("record code-review: %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatalf("record critique: %v", err)
	}
	if ok, reason := g.Check(testSHA, ""); !ok {
		t.Fatalf("want allow once both layers are ready, got BLOCK: %s", reason)
	}
}

// TestInScope_ExplicitList_AddsToOwnOrigin: a project's enforced_remotes names OTHER
// repos; it must NEVER take the project's own origin out of scope (tickets 11/87). Here
// the target IS the project's own origin (fakeGit reports one origin for the root and the
// target) and the list names a different repo entirely: still in scope, and the verdict
// is consequential — an all-zeros sha with no record is BLOCKED, not "out of scope".
func TestInScope_ExplicitList_AddsToOwnOrigin(t *testing.T) {
	g, _ := newTestGate(t, "github.com/acmecorp/widgets")
	g.EnforcedRemotes = []string{"another-project"}
	if !g.inScope("") {
		t.Fatal("an enforced_remotes list that omits the project's own origin must NOT take that origin out of scope")
	}
	ok, outcome, reason := g.CheckWithOutcome("0000000000000000000000000000000000000000", "")
	if ok || outcome != OutcomeUngated {
		t.Errorf("own origin + list naming others + no record: want BLOCK/OutcomeUngated, got ok=%v outcome=%v (%s)", ok, outcome, reason)
	}
}

// perRepoGit is a Git stub whose origin differs per repo path, so a test can tell the
// project's OWN origin (read from ProjectRoot) apart from a TARGET repo's origin — which
// fakeGit, returning one origin for everything, cannot.
type perRepoGit struct{ remotes map[string]string }

func (perRepoGit) RevParse(repo, ref string) (string, bool) { return testSHA, true }
func (p perRepoGit) RemoteURL(repo string) string           { return p.remotes[repo] }

// TestInScope_ExplicitList_StillNamesOtherRepos: the list keeps doing its own job. A
// target that is NOT the project's own origin is in scope exactly when the list
// substring-matches it, and out of scope when it does not.
func TestInScope_ExplicitList_StillNamesOtherRepos(t *testing.T) {
	g := &Gate{
		RecordsDir: t.TempDir(),
		Git: perRepoGit{remotes: map[string]string{
			"/self":  "github.com/org/self",
			"/named": "github.com/org/named-upstream",
			"/other": "github.com/org/unrelated",
		}},
		ProjectRoot:     "/self",
		EnforcedRemotes: []string{"named-upstream"},
	}
	if !g.inScope("/self") {
		t.Error("the project's own origin must be in scope")
	}
	if !g.inScope("/named") {
		t.Error("a repo the list names must be in scope")
	}
	if g.inScope("/other") {
		t.Error("a repo that is neither the project's own origin nor named by the list must be out of scope")
	}
}

// TestScopeOf_OwnOriginWithUnreadableTargetOrigin: with a list in play and an own origin
// resolvable, a target whose origin cannot be read is UNRESOLVED (enforce fails closed),
// never silently out of scope.
func TestScopeOf_OwnOriginWithUnreadableTargetOrigin(t *testing.T) {
	g := &Gate{
		RecordsDir:      t.TempDir(),
		Git:             perRepoGit{remotes: map[string]string{"/self": "github.com/org/self"}},
		ProjectRoot:     "/self",
		EnforcedRemotes: []string{"named-upstream"},
	}
	if got := g.scopeOf("/unreadable"); got != scopeUnresolved {
		t.Errorf("scopeOf(unreadable origin) = %v, want scopeUnresolved", got)
	}
}

// TestInScope_DefaultGatesOwnOrigin is the zero-config self-gating case: with
// enforced_remotes UNSET, a gate whose ProjectRoot resolves to the SAME origin as
// the target is in scope — so a project gates its OWN pushes with no named list.
// (fakeGit.RemoteURL returns the same origin for any repo, standing in for "the
// target repo IS the profile's own project.")
func TestInScope_DefaultGatesOwnOrigin(t *testing.T) {
	g, _ := newTestGate(t, "github.com/acmecorp/my-project")
	// EnforcedRemotes left UNSET (the default); ProjectRoot names the governing
	// project's own root, so ownOrigin() resolves to the same origin as the target.
	g.ProjectRoot = "/repo/my-project"

	if !g.inScope("") {
		t.Fatal("an unset enforced_remotes must GATE THE PROJECT'S OWN ORIGIN, not enforce nothing")
	}
	// The verdict is consequential: no record blocks, both layers ready passes.
	if ok, reason := g.Check(testSHA, ""); ok {
		t.Errorf("want BLOCK for the project's own repo with no record, got allow (%s)", reason)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatalf("record code-review: %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatalf("record critique: %v", err)
	}
	if ok, reason := g.Check(testSHA, ""); !ok {
		t.Fatalf("want allow once both layers are ready on the own-origin gate, got BLOCK: %s", reason)
	}
}

// TestInScope_DefaultUnprofiledTargetOutOfScope: with enforced_remotes UNSET AND no
// ProjectRoot (the config.Defaults() shape — a genuinely UNPROFILED target), no own
// origin can be derived, so the target is OUT of scope. Its own profile (if any)
// governs it; this gate never sweeps in a different, unprofiled repo.
func TestInScope_DefaultUnprofiledTargetOutOfScope(t *testing.T) {
	g, _ := newTestGate(t, "github.com/acmecorp/some-unrelated-repo")
	g.ProjectRoot = "" // EnforcedRemotes UNSET and ProjectRoot UNSET — the Defaults() shape.
	if g.inScope("") {
		t.Error("an unset list with no ProjectRoot (unprofiled target) must be OUT of scope, not swept in")
	}
}

// TestInScope_DefaultNoOwnOriginWhenRemoteUnknown: even with a ProjectRoot set, if the
// origin can't be read at all (empty URL) inScope stays false and it never matches
// "" == "". It is not "out of scope", though: a profile governs the project, so the answer
// is "cannot confirm" (OutcomeUnverifiable, which enforce mode denies), and only a repo
// with NO profile is out of scope outright.
func TestInScope_DefaultNoOwnOriginWhenRemoteUnknown(t *testing.T) {
	g, _ := newTestGate(t, "") // empty origin
	g.ProjectRoot = "/repo/my-project"
	if g.inScope("") {
		t.Error("an unknown/empty origin must not be treated as gated (no false '' == '' match)")
	}
	if got := g.scopeOf(""); got != scopeOriginUnreadable {
		t.Errorf("a profiled project whose origin cannot be read is unresolved, not out of scope: %v", got)
	}
	if _, outcome, _ := g.CheckWithOutcome(testSHA, ""); outcome != OutcomeUnverifiable {
		t.Errorf("outcome = %v, want OutcomeUnverifiable so enforce mode can deny", outcome)
	}
	g.ProjectRoot = "" // no profile: genuinely not governed
	if got := g.scopeOf(""); got != scopeOut {
		t.Errorf("an unprofiled target with no origin is out of scope: %v", got)
	}
}

// --- session enforced_remotes UNION (cross-project coverage) ---

// TestInScope_SessionListGatesUnprofiledCrossProjectTarget is the coverage-regression
// fix: an acting SESSION whose enforced_remotes names a repo pushes an UNPROFILED clone
// of it (target has no explicit list AND no own origin). Target-own gating alone reads
// it out of scope; the session list unions it back IN.
func TestInScope_SessionListGatesUnprofiledCrossProjectTarget(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	// The TARGET is unprofiled: no explicit list, no own origin.
	g.EnforcedRemotes = nil
	g.ProjectRoot = ""
	// but the acting SESSION declares it.
	g.SessionEnforcedRemotes = []string{"web-client", "platform-infra"}
	if !g.inScope("") {
		t.Fatal("a session's enforced_remotes must gate its cross-project push to an unprofiled clone")
	}
	// And the verdict is consequential: no record blocks, both layers ready passes.
	if ok, reason := g.Check(testSHA, ""); ok {
		t.Errorf("want BLOCK for a session-gated unprofiled target with no record, got allow (%s)", reason)
	}
}

// TestInScope_SessionUnionIsAddOnly proves the safety property: the session list can
// only ADD scope. A narrow/empty session list must NEVER take a target OUT of scope
// that its own config gates (the push-gate bypass fix stays intact).
func TestInScope_SessionUnionIsAddOnly(t *testing.T) {
	// Target self-gates by own origin; the session list is empty and names nothing.
	g, _ := newTestGate(t, "github.com/org/some-repo") // ProjectRoot set by newTestGate → own-origin
	g.EnforcedRemotes = nil
	g.SessionEnforcedRemotes = nil
	if !g.inScope("") {
		t.Error("an empty session list must not remove the target's own own-origin gating")
	}

	// Target self-gates by an EXPLICIT list; a session list that names a DIFFERENT repo
	// must not un-gate it.
	g2, _ := newTestGate(t, "github.com/org/target-repo")
	g2.EnforcedRemotes = []string{"target-repo"}
	g2.ProjectRoot = ""
	g2.SessionEnforcedRemotes = []string{"totally-different"}
	if !g2.inScope("") {
		t.Error("a session list naming a different repo must not un-gate a target its own explicit list gates")
	}
}

// TestInScope_EmptySessionListLeavesTargetOwnGatingUnchanged: with no session list, an
// unprofiled target stays out of scope exactly as the target-own default dictates.
func TestInScope_EmptySessionListLeavesTargetOwnGatingUnchanged(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/unrelated")
	g.EnforcedRemotes = nil
	g.ProjectRoot = "" // unprofiled target
	g.SessionEnforcedRemotes = nil
	if g.inScope("") {
		t.Error("no session list + unprofiled target must stay out of scope (union adds nothing)")
	}
}

// --- CheckUndetermined: a push whose commits cannot be named is refused, never judged by a
// borrowed sha; scope and bypass still apply. ---

func TestCheckUndetermined(t *testing.T) {
	const why = "ref \"x\" does not resolve"
	t.Run("in scope with a passing record on HEAD is still refused", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/another-project")
		g.EnforcedRemotes = []string{"another-project"}
		// Fixed order: critique is chained and may only follow a ready code-review.
		for _, r := range []struct{ layer, by string }{{"code-review", "cr-1"}, {"critique", "crit-1"}} {
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: r.layer, Verdict: "ready", By: r.by}); err != nil {
				t.Fatalf("record %s: %v", r.layer, err)
			}
		}
		if ok, _ := g.Check(testSHA, ""); !ok {
			t.Fatal("setup: HEAD should be fully gated")
		}
		ok, outcome, reason := g.CheckUndetermined("", why)
		if ok || outcome != OutcomeUngated || !strings.Contains(reason, why) {
			t.Errorf("want refusal naming %q, got ok=%v outcome=%v reason=%q", why, ok, outcome, reason)
		}
	})
	t.Run("out of scope is allowed", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/unrelated")
		g.EnforcedRemotes = []string{"another-project"}
		g.ProjectRoot = "" // no own origin: only the list can scope this repo
		if ok, outcome, _ := g.CheckUndetermined("", why); !ok || outcome != OutcomeOutOfScope {
			t.Errorf("want allowed out-of-scope, got ok=%v outcome=%v", ok, outcome)
		}
	})
	t.Run("bypass file is honoured", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/another-project")
		g.EnforcedRemotes = []string{"another-project"}
		if err := os.WriteFile(filepath.Join(g.RecordsDir, "BYPASS"), []byte("hotfix"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, outcome, _ := g.CheckUndetermined("", why); !ok || outcome != OutcomeGated {
			t.Errorf("want allowed by bypass, got ok=%v outcome=%v", ok, outcome)
		}
	})
	t.Run("bad repo is unverifiable", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/another-project")
		if _, outcome, _ := g.CheckUndetermined(filepath.Join(t.TempDir(), "missing"), why); outcome != OutcomeUnverifiable {
			t.Errorf("want OutcomeUnverifiable, got %v", outcome)
		}
	})
}
