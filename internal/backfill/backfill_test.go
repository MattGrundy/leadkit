package backfill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeAgentDef(t *testing.T, agentsDir, name string) {
	t.Helper()
	mustWrite(t, filepath.Join(agentsDir, name+".md"), "---\nname: "+name+"\n---\n")
}

func linkOverlay(t *testing.T, queueRoot, storeRoot, lane, status, base string) {
	t.Helper()
	dir := filepath.Join(queueRoot, lane, status)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(storeRoot, base))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(dir, base)); err != nil {
		t.Fatal(err)
	}
}

// fixture builds a store + queue + ROLES + team/agents/ tree covering every case
// the spec's test list names.
func fixture(t *testing.T) (queueRoot, storeRoot, rolesPath, agentsDir string) {
	t.Helper()
	root := t.TempDir()
	storeRoot = filepath.Join(root, "tasks")
	queueRoot = filepath.Join(root, "queue")
	rolesPath = filepath.Join(queueRoot, "ROLES")
	agentsDir = filepath.Join(root, "team-agents")

	for _, n := range []string{"ci-infra-agent", "devops-agent", "code-review-agent"} {
		writeAgentDef(t, agentsDir, n)
	}
	mustWrite(t, rolesPath, "code-review cr-1\n")

	// owner "ci-infra" (short form) — should collapse to ci-infra-agent.
	mustWrite(t, filepath.Join(storeRoot, "1-a.md"), "---\nid: 1\nstatus: pending\nowner: ci-infra\n---\n\nbody\n")
	// owner "ci-infra-agent" (long form) — same canonical type, from a different task.
	mustWrite(t, filepath.Join(storeRoot, "2-b.md"), "---\nid: 2\nstatus: pending\nowner: ci-infra-agent\n---\n\nbody\n")
	// owner=null — manual.
	mustWrite(t, filepath.Join(storeRoot, "3-c.md"), "---\nid: 3\nstatus: pending\nowner: null\n---\n\nbody\n")
	// a branch-name owner — manual.
	mustWrite(t, filepath.Join(storeRoot, "4-d.md"), "---\nid: 4\nstatus: pending\nowner: fix-973\n---\n\nbody\n")
	// lane-only (owner unassigned, but placed under cr-1, which ROLES maps to code-review).
	mustWrite(t, filepath.Join(storeRoot, "5-e.md"), "---\nid: 5\nstatus: in_progress\nowner: unassigned\n---\n\nbody\n")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "in-progress", "5-e.md")
	// already typed, canonical — idempotent no-op.
	mustWrite(t, filepath.Join(storeRoot, "6-f.md"), "---\nid: 6\nstatus: pending\nowner: unassigned\ntype: devops-agent\n---\n\nbody\n")

	return queueRoot, storeRoot, rolesPath, agentsDir
}

func TestComputePlan_OwnerSplitCollapsesToOneCanonicalType(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Item{}
	for _, it := range plan.Resolved {
		byID[it.ID] = it
	}
	if byID["1"].Type != "ci-infra-agent" || byID["2"].Type != "ci-infra-agent" {
		t.Fatalf("want both task 1 (owner=ci-infra) and task 2 (owner=ci-infra-agent) resolved to ci-infra-agent, got %+v %+v", byID["1"], byID["2"])
	}
}

func TestComputePlan_NullOwnerAndBranchNameAreManual(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	manualIDs := map[string]bool{}
	for _, m := range plan.Manual {
		manualIDs[m.ID] = true
	}
	if !manualIDs["3"] {
		t.Error("task 3 (owner=null) should be manual")
	}
	if !manualIDs["4"] {
		t.Error("task 4 (owner=fix-973, a branch name) should be manual")
	}
	for _, it := range plan.Resolved {
		if it.ID == "3" || it.ID == "4" {
			t.Errorf("task %s must not appear in Resolved — it should be manual, never guessed", it.ID)
		}
	}
}

func TestComputePlan_LaneOnlyTaskInfersViaROLES(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Resolved {
		if it.ID == "5" {
			if it.Type != "code-review-agent" || it.Source != "lane" {
				t.Errorf("task 5 = %+v, want type=code-review-agent source=lane", it)
			}
			return
		}
	}
	t.Error("task 5 (lane-only) should be in Resolved")
}

func TestComputePlan_AlreadyTypedIsNoOp(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range plan.Resolved {
		if it.ID == "6" {
			found = true
			if it.Source != "type" || it.Type != "devops-agent" {
				t.Errorf("task 6 = %+v, want source=type type=devops-agent", it)
			}
		}
	}
	if !found {
		t.Error("task 6 (already typed) should still appear in Resolved (as a no-op)")
	}
	if plan.AlreadyTyped() != 1 {
		t.Errorf("AlreadyTyped() = %d, want 1", plan.AlreadyTyped())
	}
}

func TestApply_DryRun_WritesNothing(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(storeRoot, "1-a.md"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := Apply(plan, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("dry-run should still report how many WOULD be written")
	}
	after, err := os.ReadFile(filepath.Join(storeRoot, "1-a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("dry-run must not write anything")
	}
}

func TestApply_WritesOnlyTypeFieldByteIdenticalOtherwise(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "backfill.jsonl")
	n, err := Apply(plan, false, logPath)
	if err != nil {
		t.Fatal(err)
	}
	// 4 resolved (1,2,5) writable + already-typed (6) is a no-op -> 3 written.
	if n != 3 {
		t.Errorf("Apply wrote %d, want 3 (task 6 is an idempotent no-op)", n)
	}

	data, err := os.ReadFile(filepath.Join(storeRoot, "1-a.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "type: ci-infra-agent") {
		t.Errorf("want a type: line written, got:\n%s", content)
	}
	for _, want := range []string{"id: 1", "status: pending", "owner: ci-infra", "body"} {
		if !strings.Contains(content, want) {
			t.Errorf("existing content changed — missing %q:\n%s", want, content)
		}
	}

	// task 6 (already typed) must be byte-identical — no write happened for it.
	orig := "---\nid: 6\nstatus: pending\nowner: unassigned\ntype: devops-agent\n---\n\nbody\n"
	got, err := os.ReadFile(filepath.Join(storeRoot, "6-f.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig {
		t.Errorf("already-typed task 6 must be untouched, got:\n%s", got)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("change log not written: %v", err)
	}
	for _, want := range []string{`"id":"1"`, `"type":"ci-infra-agent"`, `"source":"owner"`} {
		if !strings.Contains(string(logData), want) {
			t.Errorf("change log missing %q:\n%s", want, logData)
		}
	}
	if strings.Contains(string(logData), `"id":"6"`) {
		t.Error("the already-typed no-op (task 6) must not appear in the change log")
	}
}

func TestApply_SecondRunIsIdempotent(t *testing.T) {
	queueRoot, storeRoot, rolesPath, agentsDir := fixture(t)
	plan1, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(plan1, false, ""); err != nil {
		t.Fatal(err)
	}

	plan2, err := ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := Apply(plan2, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("second run should write nothing (every resolvable task is now already typed), got %d", n2)
	}
}
