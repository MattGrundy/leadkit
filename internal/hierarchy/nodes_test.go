package hierarchy

import (
	"strings"
	"testing"
)

func nodesModel(t *testing.T) (Model, []Issue, []Subtask) {
	t.Helper()
	model := Model{
		Project: "g",
		Milestones: []Milestone{
			{
				ID: "shadow-ci", Title: "Shadow CI", State: "in-progress",
				Epics: []Epic{
					{ID: "dev-portal", State: "core-done"},
					{ID: "quarantine", State: "?"},
				},
			},
		},
	}
	issues := []Issue{
		{ID: "I1", Title: "Portal issue", Status: "pending", Parent: "dev-portal"},
		{ID: "I2", Title: "Orphan issue", Status: "pending", Parent: ""}, // unmapped
	}
	subtasks := []Subtask{
		{ID: "1", Title: "Portal task", Status: "pending", Parent: "I1"},
		{ID: "2", Title: "Orphan task", Status: "pending", Parent: ""}, // unmapped
	}
	return model, issues, subtasks
}

func TestGet_ResolvesEachTaxonomyLevel(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	mr := ComputeRollup(model, issues, subtasks)

	cases := []struct {
		id   string
		kind string
	}{
		{"shadow-ci", "milestone"},
		{"dev-portal", "epic"},
		{"I1", "issue"},
		{"1", "task"},
	}
	for _, c := range cases {
		n, ok := Get(mr, subtasks, c.id)
		if !ok {
			t.Errorf("Get(%q) not found", c.id)
			continue
		}
		if n.Kind != c.kind {
			t.Errorf("Get(%q).Kind = %q, want %q", c.id, n.Kind, c.kind)
		}
	}
}

func TestGet_ResolvesUnmappedIssueAndTask(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	mr := ComputeRollup(model, issues, subtasks)

	n, ok := Get(mr, subtasks, "I2")
	if !ok || !n.Unmapped {
		t.Fatalf("Get(I2) = %+v, ok=%v, want an unmapped issue node", n, ok)
	}
	n, ok = Get(mr, subtasks, "2")
	if !ok || !n.Unmapped {
		t.Fatalf("Get(2) = %+v, ok=%v, want an unmapped task node", n, ok)
	}
}

func TestGet_UnknownIDNotFound(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	mr := ComputeRollup(model, issues, subtasks)
	if _, ok := Get(mr, subtasks, "does-not-exist"); ok {
		t.Error("want ok=false for an id that matches nothing")
	}
}

func TestList_FiltersIssuesByEpic(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	mr := ComputeRollup(model, issues, subtasks)

	nodes := List(mr, subtasks, "issue", "", "dev-portal", "")
	if len(nodes) != 1 || nodes[0].ID != "I1" {
		t.Fatalf("List(issue, epic=dev-portal) = %+v, want just I1", nodes)
	}
}

func TestList_FiltersByStatus(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	subtasks = append(subtasks, Subtask{ID: "3", Status: "completed", Parent: "I1"})
	mr := ComputeRollup(model, issues, subtasks)

	nodes := List(mr, subtasks, "task", "", "", "completed")
	if len(nodes) != 1 || nodes[0].ID != "3" {
		t.Fatalf("List(task, status=completed) = %+v, want just id 3", nodes)
	}
}

func TestList_MilestoneLevelUnfiltered(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	mr := ComputeRollup(model, issues, subtasks)
	nodes := List(mr, subtasks, "milestone", "", "", "")
	if len(nodes) != 1 || nodes[0].ID != "shadow-ci" {
		t.Fatalf("List(milestone) = %+v, want just shadow-ci", nodes)
	}
}

func TestFormatTree_ScopesToOneEpic(t *testing.T) {
	model, issues, subtasks := nodesModel(t)
	mr := ComputeRollup(model, issues, subtasks)
	out := FormatTree(mr, "", "dev-portal")
	if !strings.Contains(out, "dev-portal") || strings.Contains(out, "quarantine") {
		t.Errorf("FormatTree(epic=dev-portal) should include dev-portal and exclude quarantine, got:\n%s", out)
	}
}
