package main

// End-to-end guard test for `queue backfill-type` — same sandboxing as every other
// --apply --confirm command (see apply_guard_test.go's package doc): reuses runCLI,
// which pins HOME/cwd/LEADKIT_PROFILE unconditionally, so this can never touch the
// real store.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_QueueBackfillType_BareApplyRefusesAndNeverMutates(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	agentsDir := filepath.Join(root, "team", "agents")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "ci-infra-agent.md"), []byte("---\nname: ci-infra-agent\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(storeRoot, "1-a.md")
	original := "---\nid: 1\nstatus: pending\nowner: ci-infra\n---\n\nbody\n"
	if err := os.WriteFile(taskPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "queue", "backfill-type", "--apply")
	if code == 0 {
		t.Errorf("`queue backfill-type --apply` alone must exit non-zero, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED line, got:\n%s", out)
	}
	after, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("bare --apply must leave the task byte-for-byte unchanged\nbefore=%s\nafter=%s", original, after)
	}

	outApplied, codeApplied := runCLI(t, bin, env, "queue", "backfill-type", "--apply", "--confirm")
	if codeApplied != 0 {
		t.Fatalf("`--apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	written, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "type: ci-infra-agent") {
		t.Errorf("want type: ci-infra-agent written, got:\n%s", written)
	}
}
