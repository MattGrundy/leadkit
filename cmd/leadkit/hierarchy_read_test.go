package main

// `get`/`list`/`tree` are READ-ONLY, but every test still goes through runCLI's
// mandatory sandbox (see apply_guard_test.go's package doc) — same discipline as
// every other CLI test in this package, regardless of a command's own risk level.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReadHelperFixture(t *testing.T, root string) (profilePath string) {
	t.Helper()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(issuesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"),
		[]byte("goal: g\nmilestones:\n  - id: m1\n    title: M1\n    state: pending\n    epics:\n      - id: dev-portal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issuesRoot, "I1-x.md"),
		[]byte("---\nid: I1\ntitle: An issue\nstatus: pending\nparent: dev-portal\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "1-x.md"),
		[]byte("---\nid: 1\ntitle: A task\nstatus: pending\nparent: I1\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath = filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nissues_root: " + issuesRoot + "\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath
}

func TestCLI_Get_ResolvesEachLevel(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeReadHelperFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	for _, id := range []string{"m1", "dev-portal", "I1", "1"} {
		out, code := runCLI(t, bin, env, "get", id)
		if code != 0 {
			t.Fatalf("get %s: want exit 0, got %d. output:\n%s", id, code, out)
		}
		if !strings.Contains(out, id) {
			t.Errorf("get %s: output missing the id, got:\n%s", id, out)
		}
	}
}

func TestCLI_Get_UnknownIDRefused(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeReadHelperFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "get", "does-not-exist")
	if code == 0 {
		t.Error("want a non-zero exit for an unknown id")
	}
}

func TestCLI_List_FiltersByEpic(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeReadHelperFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "list", "--epic", "dev-portal", "--json", "issue")
	if code != 0 {
		t.Fatalf("list issue: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "I1") {
		t.Errorf("want I1 in the filtered list, got:\n%s", out)
	}
}

func TestCLI_Tree_ScopesToMilestone(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeReadHelperFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "tree", "--milestone", "m1")
	if code != 0 {
		t.Fatalf("tree: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "dev-portal") {
		t.Errorf("want dev-portal in the scoped tree, got:\n%s", out)
	}
}
