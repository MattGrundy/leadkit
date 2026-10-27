package hierarchyapply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMilestones(t *testing.T, storeRoot string) {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "project: p\nmilestones:\n  - id: m1\n    epics:\n      - id: dev-portal\n"
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTask(t *testing.T, storeRoot, filename, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(storeRoot, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func baseSetup(t *testing.T) (storeRoot, issuesRoot string) {
	t.Helper()
	root := t.TempDir()
	storeRoot = filepath.Join(root, "tasks")
	issuesRoot = filepath.Join(root, "issues")
	writeMilestones(t, storeRoot)
	return storeRoot, issuesRoot
}

func TestDryRun_PrintsPlanWritesNothing(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	writeTask(t, storeRoot, "1-a.md", "id: 1\ntitle: Task A\nstatus: pending")
	writeTask(t, storeRoot, "2-b.md", "id: 2\ntitle: Task B\nstatus: pending")

	entries := []PlanEntry{{IssueTitle: "Dev portal cleanup", Epic: "dev-portal", TaskIDs: []string{"1", "2"}}}
	plan, err := Compute(entries, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	n, err := Apply(plan, storeRoot, issuesRoot, filepath.Join(t.TempDir(), "log.jsonl"), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // 1 issue + 2 task parents
		t.Errorf("dry-run n = %d, want 3", n)
	}
	out := FormatPlan(plan, true)
	if !strings.Contains(out, "would create") || !strings.Contains(out, "would set") {
		t.Errorf("dry-run output missing preview verbs, got:\n%s", out)
	}

	entriesInIssuesDir, _ := os.ReadDir(issuesRoot)
	if len(entriesInIssuesDir) != 0 {
		t.Errorf("dry-run must write nothing to issues/, found: %v", entriesInIssuesDir)
	}
	data, _ := os.ReadFile(filepath.Join(storeRoot, "1-a.md"))
	if strings.Contains(string(data), "parent:") {
		t.Errorf("dry-run must not touch the task file, got:\n%s", data)
	}
}

func TestApply_CreatesIssueAndSetsParents(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	writeTask(t, storeRoot, "1-a.md", "id: 1\ntitle: Task A\nstatus: completed\nowner: someone")
	writeTask(t, storeRoot, "2-b.md", "id: 2\ntitle: Task B\nstatus: pending")

	entries := []PlanEntry{{IssueTitle: "Dev portal cleanup", Epic: "dev-portal", TaskIDs: []string{"1", "2"}}}
	plan, err := Compute(entries, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "log.jsonl")
	n, err := Apply(plan, storeRoot, issuesRoot, logPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}

	issueEntries, err := os.ReadDir(issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(issueEntries) != 1 {
		t.Fatalf("want exactly 1 issue file, got %v", issueEntries)
	}
	issueData, _ := os.ReadFile(filepath.Join(issuesRoot, issueEntries[0].Name()))
	for _, want := range []string{"title: Dev portal cleanup", "parent: dev-portal", "status: pending"} {
		if !strings.Contains(string(issueData), want) {
			t.Errorf("issue file missing %q:\n%s", want, issueData)
		}
	}

	taskAData, _ := os.ReadFile(filepath.Join(storeRoot, "1-a.md"))
	if !strings.Contains(string(taskAData), "parent: I") {
		t.Errorf("task 1 missing its new parent:, got:\n%s", taskAData)
	}
	if !strings.Contains(string(taskAData), "owner: someone") || !strings.Contains(string(taskAData), "status: completed") {
		t.Errorf("apply must not touch fields other than parent:, got:\n%s", taskAData)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "create_issue") || !strings.Contains(string(logData), "set_parent") {
		t.Errorf("change log missing expected entries:\n%s", logData)
	}
}

func TestApply_IssueStatusDerivation(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	writeTask(t, storeRoot, "1-a.md", "id: 1\nstatus: completed")
	writeTask(t, storeRoot, "2-b.md", "id: 2\nstatus: dropped")
	writeTask(t, storeRoot, "3-c.md", "id: 3\nstatus: pending")

	// All-terminal issue.
	allDone, err := Compute([]PlanEntry{{IssueTitle: "All done", Epic: "dev-portal", TaskIDs: []string{"1", "2"}}}, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if allDone.Issues[0].Status != "completed" {
		t.Errorf("all-terminal issue Status = %q, want completed", allDone.Issues[0].Status)
	}

	// Mixed issue.
	mixed, err := Compute([]PlanEntry{{IssueTitle: "Mixed", Epic: "dev-portal", TaskIDs: []string{"1", "3"}}}, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Issues[0].Status != "pending" {
		t.Errorf("mixed issue Status = %q, want pending", mixed.Issues[0].Status)
	}
}

func TestApply_SecondRunIsIdempotent(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	writeTask(t, storeRoot, "1-a.md", "id: 1\nstatus: pending")

	entries := []PlanEntry{{IssueTitle: "Dev portal cleanup", Epic: "dev-portal", TaskIDs: []string{"1"}}}
	logPath := filepath.Join(t.TempDir(), "log.jsonl")

	plan1, err := Compute(entries, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(plan1, storeRoot, issuesRoot, logPath, false); err != nil {
		t.Fatal(err)
	}

	// Re-run against the same plan — must reuse the existing issue, not duplicate it,
	// and must not re-tag the task (already correctly parented).
	plan2, err := Compute(entries, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !plan2.Issues[0].Reused {
		t.Error("second Compute must find and reuse the already-created issue")
	}
	if plan2.Issues[0].Tasks[0].Action != "already_set" {
		t.Errorf("second Compute's task action = %q, want already_set", plan2.Issues[0].Tasks[0].Action)
	}
	n2, err := Apply(plan2, storeRoot, issuesRoot, logPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-apply should write 0 new things, wrote %d", n2)
	}

	issueEntries, _ := os.ReadDir(issuesRoot)
	if len(issueEntries) != 1 {
		t.Errorf("want still exactly 1 issue file after re-apply, got %v", issueEntries)
	}
}

func TestCompute_TaskAlreadyParentedElsewhereIsReportedNotMoved(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	writeTask(t, storeRoot, "1-a.md", "id: 1\nstatus: pending\nparent: some-other-issue")

	entries := []PlanEntry{{IssueTitle: "Dev portal cleanup", Epic: "dev-portal", TaskIDs: []string{"1"}}}
	plan, err := Compute(entries, "", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	ta := plan.Issues[0].Tasks[0]
	if ta.Action != "skip_conflict" {
		t.Fatalf("Action = %q, want skip_conflict", ta.Action)
	}
	if _, err := Apply(plan, storeRoot, issuesRoot, filepath.Join(t.TempDir(), "log.jsonl"), false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(storeRoot, "1-a.md"))
	if !strings.Contains(string(data), "parent: some-other-issue") {
		t.Errorf("a conflicting task must never be silently re-parented, got:\n%s", data)
	}
}

func TestCompute_UnknownEpicRefusesTheWholePlan(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	entries := []PlanEntry{{IssueTitle: "x", Epic: "no-such-epic", TaskIDs: []string{"1"}}}
	if _, err := Compute(entries, "", storeRoot, issuesRoot); err == nil {
		t.Fatal("want an error for an unknown epic in the plan")
	}
}

func TestCompute_EpicFilterScopesEntries(t *testing.T) {
	storeRoot, issuesRoot := baseSetup(t)
	writeTask(t, storeRoot, "1-a.md", "id: 1\nstatus: pending")
	writeTask(t, storeRoot, "2-b.md", "id: 2\nstatus: pending")

	// Add a second epic so we have something to filter OUT.
	content := "project: p\nmilestones:\n  - id: m1\n    epics:\n      - id: dev-portal\n      - id: quarantine\n"
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := []PlanEntry{
		{IssueTitle: "Portal work", Epic: "dev-portal", TaskIDs: []string{"1"}},
		{IssueTitle: "Quarantine work", Epic: "quarantine", TaskIDs: []string{"2"}},
	}
	plan, err := Compute(entries, "dev-portal", storeRoot, issuesRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Epic != "dev-portal" {
		t.Fatalf("--epic filter should scope to just dev-portal, got %+v", plan.Issues)
	}
}

func TestParsePlan_AcceptsBareArrayAndWrappedDetailShape(t *testing.T) {
	bare := []byte(`[{"issue_title":"x","epic":"dev-portal","task_ids":["1"]}]`)
	entries, err := ParsePlan(bare)
	if err != nil || len(entries) != 1 {
		t.Fatalf("bare array: got %+v, err %v", entries, err)
	}

	wrapped := []byte(`{"detail":[{"issue_title":"y","epic":"dev-portal","task_ids":["2"]}],"needs_epic":5}`)
	entries, err = ParsePlan(wrapped)
	if err != nil || len(entries) != 1 || entries[0].IssueTitle != "y" {
		t.Fatalf("wrapped detail shape: got %+v, err %v", entries, err)
	}
}
