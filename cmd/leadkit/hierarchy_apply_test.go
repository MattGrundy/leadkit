package main

// `hierarchy apply --apply --confirm` mutates the store — every test here goes
// through runCLI's mandatory sandbox (see apply_guard_test.go's package doc) for
// exactly the same reason task create/issue create's own tests do.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHierarchyApplyFixture(t *testing.T, root string) (profilePath, planPath string) {
	t.Helper()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"),
		[]byte("project: p\nmilestones:\n  - id: m1\n    epics:\n      - id: dev-portal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "1-a.md"), []byte("---\nid: 1\ntitle: Task A\nstatus: pending\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath = filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nissues_root: " + issuesRoot + "\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	planPath = filepath.Join(root, "plan.json")
	planJSON := `[{"issue_title":"Dev portal cleanup","epic":"dev-portal","task_ids":["1"]}]`
	if err := os.WriteFile(planPath, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath, planPath
}

func TestCLI_HierarchyApply_DryRunIsDefaultAndWritesNothing(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath, planPath := writeHierarchyApplyFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "hierarchy", "apply", "--plan", planPath)
	if code != 0 {
		t.Fatalf("dry-run default: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("want the dry-run footer, got:\n%s", out)
	}
	issuesEntries, _ := os.ReadDir(filepath.Join(root, "issues"))
	if len(issuesEntries) != 0 {
		t.Errorf("dry-run must write nothing to issues/, found: %v", issuesEntries)
	}
}

func TestCLI_HierarchyApply_BareApplyRefuses(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath, planPath := writeHierarchyApplyFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "hierarchy", "apply", "--plan", planPath, "--apply")
	if code == 0 {
		t.Errorf("bare --apply must refuse (non-zero exit), got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED message, got:\n%s", out)
	}
	issuesEntries, _ := os.ReadDir(filepath.Join(root, "issues"))
	if len(issuesEntries) != 0 {
		t.Errorf("a refused bare --apply must write nothing, found: %v", issuesEntries)
	}
}

func TestCLI_HierarchyApply_ApplyConfirmWrites(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath, planPath := writeHierarchyApplyFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "hierarchy", "apply", "--plan", planPath, "--apply", "--confirm")
	if code != 0 {
		t.Fatalf("--apply --confirm: want exit 0, got %d. output:\n%s", code, out)
	}
	issuesEntries, err := os.ReadDir(filepath.Join(root, "issues"))
	if err != nil || len(issuesEntries) != 1 {
		t.Fatalf("want exactly 1 issue file written, got %v (err %v)", issuesEntries, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "tasks", "1-a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "parent: I") {
		t.Errorf("task's parent: not set after --apply --confirm, got:\n%s", data)
	}
}
