package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hierarchy"
)

func fixtureModel(t *testing.T) hierarchy.Model {
	t.Helper()
	return hierarchy.Model{
		Project: "Oldengine-off",
		Milestones: []hierarchy.Milestone{
			{
				ID: "shadow-ci", Title: "Shadow CI", State: "in-progress",
				Epics: []hierarchy.Epic{{ID: "dev-portal", State: "core-done"}},
			},
		},
	}
}

func fixtureIssuesAndSubtasks() ([]hierarchy.Issue, []hierarchy.Subtask) {
	issues := []hierarchy.Issue{
		{ID: "I1", Title: "Portal cleanup", Status: "pending", Parent: "dev-portal"},
		{ID: "I2", Title: "Run dashboard", Status: "completed", Parent: "dev-portal", Trackers: map[string]string{"linear": "LIN-2"}},
	}
	subtasks := []hierarchy.Subtask{
		{ID: "1", Title: "Task one", Status: "pending", Parent: "I1"},
		{ID: "2", Title: "Task two", Status: "completed", Parent: "I2", Trackers: map[string]string{"linear": "LIN-20"}},
		{ID: "3", Title: "Unmapped task", Status: "pending", Parent: ""},
	}
	return issues, subtasks
}

func buildFixtureSyncModel(t *testing.T) Model {
	t.Helper()
	model := fixtureModel(t)
	issues, subtasks := fixtureIssuesAndSubtasks()
	mr := hierarchy.ComputeRollup(model, issues, subtasks)
	return BuildSyncModel(mr, subtasks)
}

func TestBuildSyncModel_ParentFirstOrderAndExcludesUnmapped(t *testing.T) {
	m := buildFixtureSyncModel(t)
	var levels []string
	byID := map[string]int{} // local id -> index, to check parent-first ordering
	for i, n := range m.Nodes {
		levels = append(levels, n.Level)
		byID[n.LocalID] = i
		if n.LocalID == "3" {
			t.Error("an unmapped task must never appear in the sync model")
		}
	}
	for _, n := range m.Nodes {
		if n.ParentLocalID == "" {
			continue
		}
		if byID[n.ParentLocalID] >= byID[n.LocalID] {
			t.Errorf("node %s (parent %s) is not parent-first", n.LocalID, n.ParentLocalID)
		}
	}
	if levels[0] != "project" {
		t.Errorf("first node must be the project root, got %q", levels[0])
	}
}

func TestBuildSyncModel_CarriesTrackersAndTerminal(t *testing.T) {
	m := buildFixtureSyncModel(t)
	var task2, issue2 Node
	for _, n := range m.Nodes {
		if n.LocalID == "2" {
			task2 = n
		}
		if n.LocalID == "I2" {
			issue2 = n
		}
	}
	if task2.Trackers["linear"] != "LIN-20" || !task2.Terminal {
		t.Errorf("task 2 = %+v, want Trackers[linear]=LIN-20 and Terminal=true", task2)
	}
	if issue2.Trackers["linear"] != "LIN-2" || !issue2.Terminal {
		t.Errorf("issue I2 = %+v, want Trackers[linear]=LIN-2 and Terminal=true", issue2)
	}
}

func TestLinearAdapter_NoTrackerMeansCreateWithResolvedParent(t *testing.T) {
	m := buildFixtureSyncModel(t)
	ops, err := LinearAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	var task1Op Op
	found := false
	for _, op := range ops {
		if op.LocalID == "1" {
			task1Op, found = op, true
		}
	}
	if !found {
		t.Fatal("no op for task 1")
	}
	if task1Op.Action != "create" {
		t.Errorf("Action = %q, want create (no trackers.linear yet)", task1Op.Action)
	}
	if task1Op.ParentTrackerID != "" {
		t.Errorf("ParentTrackerID = %q, want empty (issue I1 has no linear tracker id yet either)", task1Op.ParentTrackerID)
	}
}

func TestLinearAdapter_ExistingTrackerMeansUpdateOrClose(t *testing.T) {
	m := buildFixtureSyncModel(t)
	ops, err := LinearAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Op{}
	for _, op := range ops {
		byID[op.LocalID] = op
	}
	if byID["2"].Action != "close" {
		t.Errorf("task 2 (terminal, has tracker) Action = %q, want close", byID["2"].Action)
	}
	if byID["I2"].Action != "close" {
		t.Errorf("issue I2 (terminal, has tracker) Action = %q, want close", byID["I2"].Action)
	}
}

func TestLinearAdapter_CreateResolvesParentTrackerIDWhenParentHasOne(t *testing.T) {
	// Prove the parent-resolution mechanism directly: a fresh model where the
	// CHILD has no tracker but its PARENT does.
	m2 := Model{Nodes: []Node{
		{Level: "issue", LocalID: "I9", Title: "parent", Trackers: map[string]string{"linear": "LIN-9"}},
		{Level: "task", LocalID: "9", Title: "child", ParentLocalID: "I9"},
	}}
	ops2, err := LinearAdapter{}.Plan(m2)
	if err != nil {
		t.Fatal(err)
	}
	var childOp Op
	for _, op := range ops2 {
		if op.LocalID == "9" {
			childOp = op
		}
	}
	if childOp.Action != "create" || childOp.ParentTrackerID != "LIN-9" {
		t.Errorf("child op = %+v, want create with ParentTrackerID=LIN-9", childOp)
	}
}

func TestLinearAdapter_CreatesOrderedParentFirst(t *testing.T) {
	m := buildFixtureSyncModel(t)
	ops, err := LinearAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, op := range ops {
		pos[op.LocalID] = i
	}
	if pos["dev-portal"] >= pos["I1"] {
		t.Error("epic dev-portal's op must precede issue I1's op")
	}
	if pos["I1"] >= pos["1"] {
		t.Error("issue I1's op must precede task 1's op")
	}
}

// stubAdapter is a second, trivial Output implementation — proves the interface
// (and BuildSyncModel/Model) carry nothing Linear-specific.
type stubAdapter struct{}

func (stubAdapter) Name() string { return "stub" }
func (stubAdapter) Plan(model Model) ([]Op, error) {
	ops := make([]Op, len(model.Nodes))
	for i, n := range model.Nodes {
		action := "create"
		if _, has := n.Trackers["stub"]; has {
			action = "update"
		}
		ops[i] = Op{Action: action, Level: n.Level, LocalID: n.LocalID}
	}
	return ops, nil
}

func TestOutputInterface_AdmitsASecondAdapter(t *testing.T) {
	m := buildFixtureSyncModel(t)
	var out Output = stubAdapter{}
	ops, err := out.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != len(m.Nodes) {
		t.Errorf("stub adapter produced %d ops, want %d (one per node)", len(ops), len(m.Nodes))
	}
	if out.Name() != "stub" {
		t.Errorf("Name() = %q, want stub", out.Name())
	}
}

func TestRecordID_WritesTrackersIntoFrontmatterIdempotently(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(storeRoot, "1-a.md")
	if err := os.WriteFile(taskPath, []byte("---\nid: 1\nstatus: pending\ntrackers: {}\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RecordID(storeRoot, issuesRoot, "linear", "1", "LIN-100"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "trackers: {linear: LIN-100}") {
		t.Fatalf("want trackers: {linear: LIN-100}, got:\n%s", data)
	}

	// Idempotent: recording the same id again leaves it unchanged.
	if err := RecordID(storeRoot, issuesRoot, "linear", "1", "LIN-100"); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(data2) {
		t.Errorf("re-recording the same id must be a no-op, got:\nfirst:\n%s\nsecond:\n%s", data, data2)
	}
}

func TestRecordID_UnknownLocalIDErrors(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	os.MkdirAll(storeRoot, 0o755)
	if err := RecordID(storeRoot, issuesRoot, "linear", "999", "LIN-1"); err == nil {
		t.Error("want an error for a local id with no matching Task/Issue file")
	}
}
