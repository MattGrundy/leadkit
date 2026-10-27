package queue

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/roles"
)

// setupConsolidateFixture builds a throwaway queue root — never the real queue —
// mixing proper role lanes with legacy instance/ephemeral/owner-instance lanes,
// mirroring role-keyed-lanes.md's motivating sprawl (cr-1, crit-b, fix-1161,
// owner-479a) alongside already-migrated role lanes (code-review, critique).
func setupConsolidateFixture(t *testing.T) string {
	t.Helper()
	queueRoot := t.TempDir()
	mk := func(rel string) {
		if err := os.MkdirAll(filepath.Join(queueRoot, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(rel string) {
		if err := os.WriteFile(filepath.Join(queueRoot, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("code-review/pending")
	touch("code-review/pending/100-already-role-keyed.md")

	mk("cr-1/pending")
	touch("cr-1/pending/1-a.md")
	mk("cr-1/in-progress")
	touch("cr-1/in-progress/2-b.md")

	mk("crit-b/pending")
	touch("crit-b/pending/3-c.md")

	mk("ci-infra-1/pending")
	touch("ci-infra-1/pending/4-d.md")

	mk("fix-1161/pending")
	touch("fix-1161/pending/5-e.md")

	mk("owner-479a/pending")
	touch("owner-479a/pending/6-f.md")

	return queueRoot
}

func TestComputeConsolidatePlan_PlansInstanceAndOwnerLanesToRoleLanes(t *testing.T) {
	queueRoot := setupConsolidateFixture(t)
	plan, err := ComputeConsolidatePlan(queueRoot, []string{"ci-infra"}, nil)
	if err != nil {
		t.Fatalf("ComputeConsolidatePlan: %v", err)
	}

	byFrom := map[string]ConsolidateMove{}
	for _, m := range plan.Moves {
		byFrom[m.From] = m
	}

	wantMoves := map[string]string{ // From (relative) -> ToRole
		"cr-1/pending/1-a.md":       "code-review",
		"cr-1/in-progress/2-b.md":   "code-review",
		"crit-b/pending/3-c.md":     "critique",
		"ci-infra-1/pending/4-d.md": "ci-infra",
	}
	if len(plan.Moves) != len(wantMoves) {
		t.Fatalf("Moves = %d, want %d: %+v", len(plan.Moves), len(wantMoves), plan.Moves)
	}
	for rel, wantRole := range wantMoves {
		from := filepath.Join(queueRoot, rel)
		m, ok := byFrom[from]
		if !ok {
			t.Errorf("no planned move for %s", rel)
			continue
		}
		if m.ToRole != wantRole {
			t.Errorf("move for %s: ToRole = %q, want %q", rel, m.ToRole, wantRole)
		}
		wantTo := filepath.Join(queueRoot, wantRole, m.Status, m.Name)
		if m.To != wantTo {
			t.Errorf("move for %s: To = %s, want %s", rel, m.To, wantTo)
		}
	}

	// The already-role-keyed lane must never be re-planned.
	for _, m := range plan.Moves {
		if m.FromLane == "code-review" {
			t.Errorf("code-review is already a role lane — must not be planned to move: %+v", m)
		}
	}

	sort.Strings(plan.Unmapped)
	wantUnmapped := []string{"fix-1161", "owner-479a"}
	if len(plan.Unmapped) != len(wantUnmapped) {
		t.Fatalf("Unmapped = %v, want %v", plan.Unmapped, wantUnmapped)
	}
	for i, u := range wantUnmapped {
		if plan.Unmapped[i] != u {
			t.Errorf("Unmapped[%d] = %q, want %q", i, plan.Unmapped[i], u)
		}
	}
}

func TestComputeConsolidatePlan_NoMutation(t *testing.T) {
	queueRoot := setupConsolidateFixture(t)
	if _, err := ComputeConsolidatePlan(queueRoot, []string{"ci-infra"}, nil); err != nil {
		t.Fatalf("ComputeConsolidatePlan: %v", err)
	}
	// Every original entry must still exist exactly where it was — a pure planning
	// pass touches nothing.
	for _, rel := range []string{
		"cr-1/pending/1-a.md", "cr-1/in-progress/2-b.md", "crit-b/pending/3-c.md",
		"ci-infra-1/pending/4-d.md", "fix-1161/pending/5-e.md", "owner-479a/pending/6-f.md",
		"code-review/pending/100-already-role-keyed.md",
	} {
		if _, err := os.Stat(filepath.Join(queueRoot, rel)); err != nil {
			t.Errorf("%s missing after a dry-run plan — planning must not mutate: %v", rel, err)
		}
	}
}

func TestApplyConsolidatePlan_MovesEntriesAndDrainsLegacyLanes(t *testing.T) {
	queueRoot := setupConsolidateFixture(t)
	plan, err := ComputeConsolidatePlan(queueRoot, []string{"ci-infra"}, nil)
	if err != nil {
		t.Fatalf("ComputeConsolidatePlan: %v", err)
	}

	moved, err := ApplyConsolidatePlan(plan)
	if err != nil {
		t.Fatalf("ApplyConsolidatePlan: %v", err)
	}
	if moved != len(plan.Moves) {
		t.Errorf("moved = %d, want %d", moved, len(plan.Moves))
	}

	for _, want := range []string{
		"code-review/pending/1-a.md",
		"code-review/in-progress/2-b.md",
		"critique/pending/3-c.md",
		"ci-infra/pending/4-d.md",
	} {
		if _, err := os.Stat(filepath.Join(queueRoot, want)); err != nil {
			t.Errorf("%s not found after apply: %v", want, err)
		}
	}

	// The legacy lanes must now be drained (their entries gone), even though the
	// directories themselves are left in place.
	for _, drained := range []string{"cr-1/pending/1-a.md", "cr-1/in-progress/2-b.md", "crit-b/pending/3-c.md", "ci-infra-1/pending/4-d.md"} {
		if _, err := os.Lstat(filepath.Join(queueRoot, drained)); err == nil {
			t.Errorf("%s should have been moved out, but still exists", drained)
		}
	}

	// Unmapped/ephemeral lanes are untouched by apply.
	for _, untouched := range []string{"fix-1161/pending/5-e.md", "owner-479a/pending/6-f.md"} {
		if _, err := os.Stat(filepath.Join(queueRoot, untouched)); err != nil {
			t.Errorf("%s should be untouched (unmappable) but is missing: %v", untouched, err)
		}
	}
}

func TestApplyConsolidatePlan_IdempotentOnRerunAfterPartialApply(t *testing.T) {
	queueRoot := setupConsolidateFixture(t)
	plan, err := ComputeConsolidatePlan(queueRoot, []string{"ci-infra"}, nil)
	if err != nil {
		t.Fatalf("ComputeConsolidatePlan: %v", err)
	}
	if _, err := ApplyConsolidatePlan(plan); err != nil {
		t.Fatalf("first ApplyConsolidatePlan: %v", err)
	}

	// Re-running the SAME (now-stale) plan a second time must not error — every
	// source is already gone, so every move is simply skipped.
	moved, err := ApplyConsolidatePlan(plan)
	if err != nil {
		t.Fatalf("second ApplyConsolidatePlan: %v", err)
	}
	if moved != 0 {
		t.Errorf("second apply moved = %d, want 0 (idempotent no-op)", moved)
	}
}

// TestComputeConsolidatePlan_AgentFoldAndAliasLabeled proves `queue consolidate`'s
// dry-run maps an `-agent` variant lane AND an alias-only lane, labels HOW each
// mapped ("via -agent fold" / "via alias"), and leaves a `_held` holding-pen lane
// unmapped by design.
func TestComputeConsolidatePlan_AgentFoldAndAliasLabeled(t *testing.T) {
	queueRoot := t.TempDir()
	mk := func(rel string) {
		if err := os.MkdirAll(filepath.Join(queueRoot, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(rel string) {
		if err := os.WriteFile(filepath.Join(queueRoot, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// devops-agent maps via the built-in -agent fold (no alias needed).
	mk("devops-agent/pending")
	touch("devops-agent/pending/10-a.md")
	// stray-lane maps ONLY via an explicit alias.
	mk("stray-lane/pending")
	touch("stray-lane/pending/11-b.md")
	// _held is a holding pen — stays unmapped by design.
	mk("_held/pending")
	touch("_held/pending/12-c.md")

	known := roles.KnownRoles([]string{"devops"})
	aliases := map[string]string{"stray-lane": "devops"}
	plan, err := ComputeConsolidatePlan(queueRoot, known, aliases)
	if err != nil {
		t.Fatalf("ComputeConsolidatePlan: %v", err)
	}

	byLane := map[string]ConsolidateMove{}
	for _, m := range plan.Moves {
		byLane[m.FromLane] = m
	}
	if m, ok := byLane["devops-agent"]; !ok || m.ToRole != "devops" || m.Via != roles.LaneAgentFold {
		t.Errorf("devops-agent move = %+v, want ToRole=devops Via=LaneAgentFold", m)
	}
	if m, ok := byLane["stray-lane"]; !ok || m.ToRole != "devops" || m.Via != roles.LaneAlias {
		t.Errorf("stray-lane move = %+v, want ToRole=devops Via=LaneAlias", m)
	}

	// _held stays unmapped by design.
	if len(plan.Unmapped) != 1 || plan.Unmapped[0] != "_held" {
		t.Errorf("Unmapped = %v, want [_held]", plan.Unmapped)
	}

	// The dry-run output labels HOW each lane mapped.
	out := FormatConsolidatePlan(plan, false)
	if !strings.Contains(out, "(via -agent fold)") {
		t.Errorf("dry-run missing -agent-fold label:\n%s", out)
	}
	if !strings.Contains(out, "(via alias)") {
		t.Errorf("dry-run missing alias label:\n%s", out)
	}
	if !strings.Contains(out, "_held") {
		t.Errorf("dry-run must still report _held as unmappable:\n%s", out)
	}
}

func TestFormatConsolidatePlan_DryRunVsApplied(t *testing.T) {
	plan := ConsolidatePlan{
		Moves:    []ConsolidateMove{{FromLane: "cr-1", ToRole: "code-review", Status: "pending", Name: "1-a.md"}},
		Unmapped: []string{"fix-1161"},
	}
	dry := FormatConsolidatePlan(plan, false)
	if !strings.Contains(dry, "DRY RUN") || !strings.Contains(dry, "would move") || !strings.Contains(dry, "fix-1161") {
		t.Errorf("dry-run format missing expected markers:\n%s", dry)
	}
	applied := FormatConsolidatePlan(plan, true)
	if !strings.Contains(applied, "APPLIED") || !strings.Contains(applied, "moved cr-1") {
		t.Errorf("applied format missing expected markers:\n%s", applied)
	}
}
