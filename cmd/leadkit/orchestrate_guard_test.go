package main

// End-to-end guard test for `orchestrate apply` — same sandboxing as init/audit/team
// install (see apply_guard_test.go's package doc): reuses runCLI, which pins
// HOME/cwd/LEADKIT_PROFILE unconditionally, so this can never touch a real queue.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_OrchestrateApply_BareApplyRefusesAndNeverMutates(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(queueRoot, "code-review", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "10-ready.md"), []byte("---\nid: 10\nstatus: pending\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolesPath := filepath.Join(queueRoot, "ROLES")
	if err := os.WriteFile(rolesPath, []byte("code-review cr-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	planPath := filepath.Join(root, "plan.json")
	planJSON := `{"route":[{"task_id":"10","agent":"cr-1","type":"code-review"}],"requeue":[],"capacity":[],"escalate":[],"triage":[]}`
	if err := os.WriteFile(planPath, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nroles_path: queue/ROLES\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	before := snapshotFiles(t, root)

	out, code := runCLI(t, bin, env, "orchestrate", "apply", "--plan", planPath, "--apply")
	if code == 0 {
		t.Errorf("`orchestrate apply --apply` alone must exit non-zero, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED line, got:\n%s", out)
	}
	after := snapshotFiles(t, root)
	if len(before) != len(after) {
		t.Fatalf("bare --apply must not touch anything\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "code-review", "pending", "10-ready.md")); !os.IsNotExist(err) {
		t.Error("bare --apply must not have routed task 10")
	}

	outApplied, codeApplied := runCLI(t, bin, env, "orchestrate", "apply", "--plan", planPath, "--apply", "--confirm")
	if codeApplied != 0 {
		t.Fatalf("`--apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "code-review", "pending", "10-ready.md")); err != nil {
		t.Error("task 10 should now be routed")
	}
}
