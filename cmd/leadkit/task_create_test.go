package main

// `leadkit task create` is NOT confirm-guarded (a normal workflow op) — which makes
// the sandboxing in runCLI (HOME/cwd/LEADKIT_PROFILE, see apply_guard_test.go's
// package doc) the ONLY thing standing between a test/dogfood run and adding junk to
// the real live store. Every test here goes through runCLI for exactly that reason.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_TaskCreate_WritesIntoTempStoreOnly(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(agentsDir, "devops-agent.md"), []byte("---\nname: devops-agent\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "task", "create", "--title", "Dogfood task", "--type", "devops-agent", "--unparented")
	if code != 0 {
		t.Fatalf("want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "created:") {
		t.Errorf("want a created: line, got:\n%s", out)
	}
	if strings.TrimSpace(out) == "" || !strings.HasSuffix(strings.TrimSpace(out), "1") {
		t.Errorf("want the id printed last/alone, got:\n%s", out)
	}

	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "1-dogfood-task") {
		t.Fatalf("want exactly 1-dogfood-task*.md in the temp store, got %v", entries)
	}

	data, err := os.ReadFile(filepath.Join(storeRoot, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id: 1", "title: Dogfood task", "status: pending", "type: devops-agent", "trackers: {}"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("frontmatter missing %q:\n%s", want, data)
		}
	}
}

func TestCLI_TaskCreate_NonCanonicalTypeRefused(t *testing.T) {
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

	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "task", "create", "--title", "x", "--type", "bogus")
	if code == 0 {
		t.Error("want a non-zero exit for a non-canonical --type")
	}
	entries, _ := os.ReadDir(storeRoot)
	if len(entries) != 0 {
		t.Errorf("refused creation must write nothing to the temp store, found: %v", entries)
	}
}

func TestCLI_TaskCreate_RequiresIssueUnlessUnparented(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(agentsDir, "devops-agent.md"), []byte("---\nname: devops-agent\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nissues_root: issues\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "task", "create", "--title", "x", "--type", "devops-agent")
	if code == 0 {
		t.Error("want a non-zero exit when --issue is omitted without --unparented")
	}
	entries, _ := os.ReadDir(storeRoot)
	if len(entries) != 0 {
		t.Errorf("refused creation must write nothing, found: %v", entries)
	}
}

func TestCLI_IssueCreateThenTaskCreateWithIssue(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	agentsDir := filepath.Join(root, "team", "agents")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "devops-agent.md"), []byte("---\nname: devops-agent\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"),
		[]byte("goal: g\nmilestones:\n  - id: m1\n    title: M1\n    state: pending\n    epics:\n      - id: dev-portal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nissues_root: " + issuesRoot + "\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "issue", "create", "--title", "New issue", "--epic", "dev-portal")
	if code != 0 {
		t.Fatalf("issue create: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "I1") {
		t.Fatalf("want the new issue id I1 printed, got:\n%s", out)
	}

	out, code = runCLI(t, bin, env, "task", "create", "--title", "Dogfood task", "--type", "devops-agent", "--issue", "I1")
	if code != 0 {
		t.Fatalf("task create --issue: want exit 0, got %d. output:\n%s", code, out)
	}
	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	var taskFile string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "1-dogfood-task") {
			taskFile = e.Name()
		}
	}
	if taskFile == "" {
		t.Fatalf("want a 1-dogfood-task*.md in the temp store, got %v", entries)
	}
	data, err := os.ReadFile(filepath.Join(storeRoot, taskFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "parent: I1") {
		t.Errorf("want parent: I1 in the new task's frontmatter, got:\n%s", data)
	}
}

func TestCLI_TaskCreate_DanglingIssueRefused(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(agentsDir, "devops-agent.md"), []byte("---\nname: devops-agent\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nissues_root: issues\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "task", "create", "--title", "x", "--type", "devops-agent", "--issue", "I999")
	if code == 0 {
		t.Error("want a non-zero exit for a dangling --issue")
	}
	entries, _ := os.ReadDir(storeRoot)
	if len(entries) != 0 {
		t.Errorf("refused creation must write nothing, found: %v", entries)
	}
}
