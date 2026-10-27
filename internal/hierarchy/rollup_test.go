package hierarchy

import "testing"

func sampleModel(t *testing.T) Model {
	t.Helper()
	return Model{
		Project: "g",
		Milestones: []Milestone{
			{
				ID: "shadow-ci", Title: "Shadow CI", State: "in-progress",
				Epics: []Epic{
					{ID: "dev-portal", State: "core-done"},
					{
						ID: "quarantine", State: "?",
						Tasks: []Task{{ID: "allure"}, {ID: "quarantine-heuristic"}},
					},
				},
			},
		},
	}
}

func TestComputeRollup_FourteenOfFifteenReadsInProgressNotComplete(t *testing.T) {
	model := sampleModel(t)
	var subtasks []Subtask
	for i := 0; i < 14; i++ {
		subtasks = append(subtasks, Subtask{ID: string(rune('a' + i)), Status: "completed", Parent: "dev-portal"})
	}
	subtasks = append(subtasks, Subtask{ID: "z", Status: "in_progress", Parent: "dev-portal"})

	mr := ComputeRollup(model, nil, subtasks)
	epic := mr.Milestones[0].Epics[0]
	if epic.Rollup.Total != 15 {
		t.Fatalf("Total = %d, want 15", epic.Rollup.Total)
	}
	if epic.Rollup.Terminal() != 14 {
		t.Errorf("Terminal() = %d, want 14", epic.Rollup.Terminal())
	}
	if epic.Rollup.Complete {
		t.Error("14/15 terminal must read as NOT complete (in-progress)")
	}
}

func TestComputeRollup_AllTerminalIsComplete(t *testing.T) {
	model := sampleModel(t)
	subtasks := []Subtask{
		{ID: "1", Status: "completed", Parent: "dev-portal"},
		{ID: "2", Status: "dropped", Parent: "dev-portal"},
	}
	mr := ComputeRollup(model, nil, subtasks)
	epic := mr.Milestones[0].Epics[0]
	if !epic.Rollup.Complete {
		t.Errorf("want Complete=true when every child is terminal, got %+v", epic.Rollup)
	}
}

func TestComputeRollup_EmptyEpicIsNotComplete(t *testing.T) {
	model := sampleModel(t)
	mr := ComputeRollup(model, nil, nil)
	epic := mr.Milestones[0].Epics[0]
	if epic.Rollup.Complete {
		t.Error("an epic with zero mapped subtasks must not read as complete")
	}
}

func TestComputeRollup_NestedTaskRollsUpIntoEpic(t *testing.T) {
	model := sampleModel(t)
	subtasks := []Subtask{
		{ID: "1", Status: "pending", Parent: "allure"},
		{ID: "2", Status: "completed", Parent: "quarantine-heuristic"},
	}
	mr := ComputeRollup(model, nil, subtasks)
	quarantine := mr.Milestones[0].Epics[1]
	if quarantine.Rollup.Total != 2 {
		t.Fatalf("quarantine epic Total = %d, want 2 (merged from both nested tasks)", quarantine.Rollup.Total)
	}
	if len(quarantine.Tasks) != 2 || quarantine.Tasks[0].Rollup.Total != 1 || quarantine.Tasks[1].Rollup.Total != 1 {
		t.Errorf("nested task rollups = %+v, want 1 each", quarantine.Tasks)
	}
}

func TestComputeRollup_UnmappedDetection(t *testing.T) {
	model := sampleModel(t)
	subtasks := []Subtask{
		{ID: "1", Status: "pending", Parent: ""},             // no parent at all
		{ID: "2", Status: "pending", Parent: "no-such-epic"}, // dangling parent
		{ID: "3", Status: "pending", Parent: "dev-portal"},   // mapped
	}
	mr := ComputeRollup(model, nil, subtasks)
	if len(mr.Unmapped) != 2 {
		t.Fatalf("Unmapped = %v, want 2 entries (ids 1 and 2)", mr.Unmapped)
	}
	for _, id := range []string{"1", "2"} {
		found := false
		for _, u := range mr.Unmapped {
			if u == id {
				found = true
			}
		}
		if !found {
			t.Errorf("Unmapped missing %q: %v", id, mr.Unmapped)
		}
	}
}

func TestComputeRollup_MilestoneAggregatesAcrossEpics(t *testing.T) {
	model := sampleModel(t)
	subtasks := []Subtask{
		{ID: "1", Status: "completed", Parent: "dev-portal"},
		{ID: "2", Status: "pending", Parent: "allure"},
	}
	mr := ComputeRollup(model, nil, subtasks)
	if mr.Milestones[0].Rollup.Total != 2 {
		t.Errorf("milestone Total = %d, want 2 (1 dev-portal + 1 quarantine/allure)", mr.Milestones[0].Rollup.Total)
	}
}

func TestComputeRollup_IssueLevelFourteenOfFifteenReadsInProgress(t *testing.T) {
	model := sampleModel(t)
	issues := []Issue{{ID: "I1", Title: "Issue one", Parent: "dev-portal"}}
	var subtasks []Subtask
	for i := 0; i < 14; i++ {
		subtasks = append(subtasks, Subtask{ID: string(rune('a' + i)), Status: "completed", Parent: "I1"})
	}
	subtasks = append(subtasks, Subtask{ID: "z", Status: "in_progress", Parent: "I1"})

	mr := ComputeRollup(model, issues, subtasks)
	epic := mr.Milestones[0].Epics[0]
	if len(epic.Issues) != 1 {
		t.Fatalf("want 1 issue under dev-portal, got %+v", epic.Issues)
	}
	issueRoll := epic.Issues[0]
	if issueRoll.Rollup.Total != 15 || issueRoll.Rollup.Terminal() != 14 {
		t.Fatalf("issue rollup = %+v, want 14/15 terminal", issueRoll.Rollup)
	}
	if issueRoll.Rollup.Complete {
		t.Error("14/15 terminal must read as NOT complete (in-progress) at the Issue level")
	}
	// The epic must reflect the same 14/15 (merged up from its one issue).
	if epic.Rollup.Total != 15 || epic.Rollup.Terminal() != 14 || epic.Rollup.Complete {
		t.Errorf("epic rollup = %+v, want 14/15 terminal, not complete (merged from its Issue)", epic.Rollup)
	}
}

func TestComputeRollup_UnmappedIssueDetected(t *testing.T) {
	model := sampleModel(t)
	issues := []Issue{
		{ID: "I1", Parent: ""},             // no parent at all
		{ID: "I2", Parent: "no-such-epic"}, // dangling parent
		{ID: "I3", Parent: "dev-portal"},   // mapped
	}
	mr := ComputeRollup(model, issues, nil)
	if len(mr.UnmappedIssues) != 2 {
		t.Fatalf("UnmappedIssues = %+v, want 2 entries (I1 and I2)", mr.UnmappedIssues)
	}
	for _, id := range []string{"I1", "I2"} {
		found := false
		for _, u := range mr.UnmappedIssues {
			if u.Issue.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("UnmappedIssues missing %q: %+v", id, mr.UnmappedIssues)
		}
	}
}

func TestComputeRollup_TaskParentedToIssueIsNoLongerUnmapped(t *testing.T) {
	model := sampleModel(t)
	issues := []Issue{{ID: "I1", Parent: "dev-portal"}}
	subtasks := []Subtask{{ID: "1", Status: "pending", Parent: "I1"}}
	mr := ComputeRollup(model, issues, subtasks)
	if len(mr.Unmapped) != 0 {
		t.Errorf("a task parented to a real issue must not be Unmapped, got %v", mr.Unmapped)
	}
	if mr.Milestones[0].Epics[0].Issues[0].Rollup.Total != 1 {
		t.Errorf("want the task rolled up into its issue, got %+v", mr.Milestones[0].Epics[0].Issues[0].Rollup)
	}
}
