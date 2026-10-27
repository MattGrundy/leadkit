package roles

import (
	"reflect"
	"testing"
)

// TestCanonicalLane covers exactly the matrix role-keyed-lanes.md's spec calls
// out: instance names for the three short-form gate roles, an owner-instance
// resolving to its declared owner role, a fixed role resolving to itself, and an
// un-mappable ephemeral name being flagged rather than silently given its own
// lane.
func TestCanonicalLane(t *testing.T) {
	known := []string{"ci-infra", "devops", "qa"}

	tests := []struct {
		name string
		want string
		ok   bool
	}{
		{"cr-1-3", "code-review", true},
		{"crit-2r", "critique", true},
		{"code-review", "code-review", true},
		{"critique", "critique", true},
		{"finalise", "finalise", true},
		{"finalise-1", "finalise", true},
		{"orchestrator", "orchestrator", true},
		{"ci-infra-1", "ci-infra", true}, // an owner-instance -> its owner role
		{"ci-infra", "ci-infra", true},   // a project role -> itself
		{"fix-1161", "", false},          // ephemeral per-task lane -> flagged
		{"spike-merge", "", false},       // ephemeral spike lane -> flagged
		{"owner-479a", "", false},        // ephemeral per-owner-instance -> flagged
	}
	for _, tc := range tests {
		got, ok := CanonicalLane(tc.name, known, nil)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CanonicalLane(%q) = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCanonicalLane_NilKnownStillResolvesFixedRoles proves FixedRoles is always
// implicit — a caller with no project roster at all (known=nil) still resolves
// every fixed review-gate role and its instance names.
func TestCanonicalLane_NilKnownStillResolvesFixedRoles(t *testing.T) {
	for _, name := range []string{"cr-1", "crit-2", "finalise", "orchestrator"} {
		if _, ok := CanonicalLane(name, nil, nil); !ok {
			t.Errorf("CanonicalLane(%q, nil) ok=false, want true", name)
		}
	}
	if _, ok := CanonicalLane("ci-infra-1", nil, nil); ok {
		t.Error("CanonicalLane(\"ci-infra-1\", nil) ok=true, want false — ci-infra is a project role, not fixed")
	}
}

// TestCanonicalLane_NeverDisagreesWithGateCanonicalRole is the reconciliation
// regression: for every short-form family, CanonicalLane's answer folds through
// exactly the same family gate.CanonicalRole assigns `by` to — the two must never
// disagree about what counts as the same role (role-keyed-lanes.md's hard
// constraint: moving tasks to role lanes must not change any gate decision).
func TestCanonicalLane_NeverDisagreesWithGateCanonicalRole(t *testing.T) {
	pairs := map[string]string{"cr-9": "code-review", "crit-9r": "critique"}
	for name, wantRole := range pairs {
		role, ok := CanonicalLane(name, nil, nil)
		if !ok || role != wantRole {
			t.Errorf("CanonicalLane(%q) = (%q, %v), want (%q, true)", name, role, ok, wantRole)
		}
	}
}

// TestCanonicalLane_AgentFoldFromRosterAlone proves the `-agent` LANE-fold maps
// #123's `<role>-agent` variant lanes onto their roster role with ZERO per-lane
// config (no lane_aliases): devops-agent→devops, qa-agent→qa,
// ci-pipeline-agent→ci-pipeline — all from the roster alone, via LaneAgentFold.
func TestCanonicalLane_AgentFoldFromRosterAlone(t *testing.T) {
	known := KnownRoles([]string{"devops", "qa", "ci-pipeline"})
	cases := map[string]string{
		"devops-agent":      "devops",
		"qa-agent":          "qa",
		"ci-pipeline-agent": "ci-pipeline",
	}
	for lane, wantRole := range cases {
		role, via, ok := ResolveLane(lane, known, nil)
		if !ok || role != wantRole {
			t.Errorf("ResolveLane(%q) = (%q, %v), want (%q, true)", lane, role, ok, wantRole)
		}
		if via != LaneAgentFold {
			t.Errorf("ResolveLane(%q) via = %v, want LaneAgentFold", lane, via)
		}
	}
}

// TestCanonicalLane_ExplicitAgentRoleNotHijacked proves step 1 wins: a roster role
// literally named "foo-agent" resolves to "foo-agent" itself (LaneCanonical) — the
// `-agent` strip must NOT hijack an explicitly-declared role that really ends in
// -agent.
func TestCanonicalLane_ExplicitAgentRoleNotHijacked(t *testing.T) {
	known := KnownRoles([]string{"foo-agent"})
	role, via, ok := ResolveLane("foo-agent", known, nil)
	if !ok || role != "foo-agent" {
		t.Fatalf("ResolveLane(%q) = (%q, %v), want (%q, true)", "foo-agent", role, ok, "foo-agent")
	}
	if via != LaneCanonical {
		t.Errorf("ResolveLane(%q) via = %v, want LaneCanonical (step 1 wins, not the strip)", "foo-agent", via)
	}
}

// TestCanonicalLane_ExplicitAliasStillWorks proves an explicit lane_aliases entry
// still maps (even though the `-agent` fold now makes it redundant): with roster
// [devops] and lane_aliases {devops-agent: devops}, devops-agent resolves to devops.
// (Here the fold catches it first, so via is LaneAgentFold — the alias is a no-op
// safety net; TestCanonicalLane_FoldedVariantAliases exercises the alias path.)
func TestCanonicalLane_ExplicitAliasStillWorks(t *testing.T) {
	known := KnownRoles([]string{"devops"})
	role, ok := CanonicalLane("devops-agent", known, map[string]string{"devops-agent": "devops"})
	if !ok || role != "devops" {
		t.Fatalf("CanonicalLane(devops-agent, alias) = (%q, %v), want (devops, true)", role, ok)
	}
}

// TestCanonicalLane_FoldedVariantAliases proves a folded variant the `-agent` strip
// can't rescue routes through lane_aliases on its FOLDED name: devops-agent-2 folds
// (instance-counter) to devops-agent, which the alias maps to devops — via LaneAlias.
func TestCanonicalLane_FoldedVariantAliases(t *testing.T) {
	known := KnownRoles([]string{"devops"})
	aliases := map[string]string{"devops-agent": "devops"}
	role, via, ok := ResolveLane("devops-agent-2", known, aliases)
	if !ok || role != "devops" {
		t.Fatalf("ResolveLane(devops-agent-2) = (%q, %v), want (devops, true)", role, ok)
	}
	if via != LaneAlias {
		t.Errorf("ResolveLane(devops-agent-2) via = %v, want LaneAlias", via)
	}
}

// TestCanonicalLane_DanglingAliasRejected proves an alias whose TARGET isn't a known
// role is REFUSED (ok=false, no silent phantom lane) and surfaced by
// InvalidLaneAliases for doctor.
func TestCanonicalLane_DanglingAliasRejected(t *testing.T) {
	known := KnownRoles([]string{"devops"})
	aliases := map[string]string{"stray-lane": "not-a-real-role"}
	if role, ok := CanonicalLane("stray-lane", known, aliases); ok {
		t.Errorf("CanonicalLane(stray-lane, dangling alias) = (%q, true), want ok=false (no phantom lane)", role)
	}
	bad := InvalidLaneAliases(aliases, known)
	if !reflect.DeepEqual(bad, []string{"stray-lane"}) {
		t.Errorf("InvalidLaneAliases = %v, want [stray-lane]", bad)
	}
	// a valid-target alias is not flagged.
	if bad := InvalidLaneAliases(map[string]string{"devops-agent": "devops"}, known); len(bad) != 0 {
		t.Errorf("InvalidLaneAliases(valid) = %v, want empty", bad)
	}
}

// TestCanonicalLane_AliasNeverOverridesCanonical proves an alias can't redirect a
// name that already resolves canonically: code-review stays code-review even when an
// alias names it (step 1 wins, the alias is never consulted).
func TestCanonicalLane_AliasNeverOverridesCanonical(t *testing.T) {
	known := KnownRoles([]string{"devops"})
	aliases := map[string]string{"code-review": "devops"}
	role, via, ok := ResolveLane("code-review", known, aliases)
	if !ok || role != "code-review" {
		t.Fatalf("ResolveLane(code-review, override alias) = (%q, %v), want (code-review, true)", role, ok)
	}
	if via != LaneCanonical {
		t.Errorf("ResolveLane(code-review) via = %v, want LaneCanonical (alias must not override)", via)
	}
}

// TestCanonicalLane_AbsentAliasesIdenticalToBaseline proves an absent lane_aliases
// map (nil) leaves resolution byte-identical to the pre-aliases behavior: fixed +
// roster + instance folds still resolve, and an ephemeral name with no alias is
// still ok=false — no regression, no accidental mapping.
func TestCanonicalLane_AbsentAliasesIdenticalToBaseline(t *testing.T) {
	known := KnownRoles([]string{"ci-infra", "devops", "qa"})
	for _, name := range []string{"cr-1-3", "ci-infra-1", "devops", "finalise-1"} {
		if _, ok := CanonicalLane(name, known, nil); !ok {
			t.Errorf("CanonicalLane(%q, nil aliases) ok=false, want true", name)
		}
	}
	for _, name := range []string{"fix-1161", "spike-merge", "owner-479a"} {
		if role, ok := CanonicalLane(name, known, nil); ok {
			t.Errorf("CanonicalLane(%q, nil aliases) = (%q, true), want ok=false (ephemeral, unmapped)", name, role)
		}
	}
}

func TestKnownRoles(t *testing.T) {
	got := KnownRoles([]string{"devops", "code-review", "qa", ""})
	want := append(append([]string{}, FixedRoles...), "devops", "qa")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("KnownRoles = %v, want %v (FixedRoles first, deduped, empty entries dropped)", got, want)
	}
}

// The review gate has two layers and no lane serves a third, so the retired outcome-verifier is
// not an implicit canonical role: an adopter told it was canonical would wait for a verdict
// nothing records. A project that DECLARES it in its roster still resolves (the role is then its
// own, not the fixed set's).
func TestFixedRoles_DoNotNameTheRetiredOutcomeVerifier(t *testing.T) {
	for _, r := range KnownRoles(nil) {
		if r == "outcome-verifier" {
			t.Fatalf("KnownRoles(nil) = %v, must not include the retired outcome-verifier", KnownRoles(nil))
		}
	}
	for _, name := range []string{"outcome-verifier", "ov", "ov-4"} {
		if role, ok := CanonicalLane(name, nil, nil); ok {
			t.Errorf("CanonicalLane(%q, nil) = %q, want no match", name, role)
		}
	}
	if role, ok := CanonicalLane("outcome-verifier", KnownRoles([]string{"outcome-verifier"}), nil); !ok || role != "outcome-verifier" {
		t.Errorf("a roster-declared outcome-verifier must still resolve, got (%q, %v)", role, ok)
	}
}
