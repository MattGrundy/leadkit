package hierarchy

import "testing"

func mkItem(id, title, status string) taskItem {
	return taskItem{Subtask: Subtask{ID: id, Title: title, Status: status}, tokens: tokenizeTitle(title)}
}

func TestClusterEpicTasks_SharedThemeFormsOneCandidate(t *testing.T) {
	items := []taskItem{
		mkItem("1", "registry cache warmup", "pending"),
		mkItem("2", "registry cache invalidation", "pending"),
		mkItem("3", "registry cache eviction policy", "pending"),
	}
	candidates := clusterEpicTasks("some-epic", items, 2)
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate (shared theme), got %d: %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if c.Count != 3 {
		t.Errorf("Count = %d, want 3", c.Count)
	}
	if c.SuggestedName != "registry-cache" {
		t.Errorf("SuggestedName = %q, want registry-cache", c.SuggestedName)
	}
}

func TestClusterEpicTasks_DistinctThemeIsASeparateCandidate(t *testing.T) {
	items := []taskItem{
		mkItem("1", "registry cache warmup", "pending"),
		mkItem("2", "registry cache invalidation", "pending"),
		mkItem("3", "multiarch build support arm64", "pending"),
		mkItem("4", "multiarch build support amd64", "pending"),
	}
	candidates := clusterEpicTasks("some-epic", items, 2)
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidates (two distinct themes), got %d: %+v", len(candidates), candidates)
	}
	names := map[string]bool{candidates[0].SuggestedName: true, candidates[1].SuggestedName: true}
	if !names["registry-cache"] || !names["multiarch-build"] {
		t.Errorf("candidates = %+v, want registry-cache and multiarch-build", candidates)
	}
}

func TestClusterEpicTasks_LoneTaskIsSingleton(t *testing.T) {
	items := []taskItem{mkItem("1", "completely unrelated one-off thing", "pending")}
	candidates := clusterEpicTasks("some-epic", items, 2)
	if len(candidates) != 1 || candidates[0].Count != 1 {
		t.Fatalf("want 1 singleton candidate, got %+v", candidates)
	}
}

func TestClusterEpicTasks_DoneActiveSplitCounted(t *testing.T) {
	items := []taskItem{
		mkItem("1", "registry cache warmup", "completed"),
		mkItem("2", "registry cache invalidation", "completed"),
		mkItem("3", "registry cache eviction", "pending"),
	}
	candidates := clusterEpicTasks("some-epic", items, 2)
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %+v", candidates)
	}
	if candidates[0].DoneCount != 2 || candidates[0].ActiveCount != 1 {
		t.Errorf("Done/Active = %d/%d, want 2/1", candidates[0].DoneCount, candidates[0].ActiveCount)
	}
}

func clusterModel(t *testing.T) Model {
	t.Helper()
	return Model{
		Milestones: []Milestone{
			{ID: "m", Epics: []Epic{{ID: "release-build"}, {ID: "quarantine"}}},
		},
	}
}

func TestClusterIssues_TaskWithNoProposedEpicNeedsEpicNotClustered(t *testing.T) {
	model := clusterModel(t)
	subtasks := []Subtask{
		{ID: "1", Title: "multiarch build fix", Status: "pending"},                // keyword -> release-build
		{ID: "2", Title: "completely unrelated one-off thing", Status: "pending"}, // no signal -> future-work
	}
	res := ClusterIssues(model, subtasks, "", 2, testHints())
	if res.NeedsEpic != 1 {
		t.Errorf("NeedsEpic = %d, want 1", res.NeedsEpic)
	}
	for _, s := range res.Summary {
		if s.Epic == FutureWorkEpic {
			t.Errorf("future-work must never appear as a clustered epic in the summary, got %+v", s)
		}
	}
}

func TestClusterIssues_SummaryModeOmitsDetail(t *testing.T) {
	model := clusterModel(t)
	subtasks := []Subtask{
		{ID: "1", Title: "multiarch build fix arm64", Status: "pending"},
		{ID: "2", Title: "multiarch build fix amd64", Status: "pending"},
	}
	res := ClusterIssues(model, subtasks, "", 2, testHints())
	if len(res.Detail) != 0 {
		t.Errorf("summary mode must not populate Detail, got %+v", res.Detail)
	}
	if len(res.Summary) != 1 || res.Summary[0].Epic != "release-build" || res.Summary[0].Candidates != 1 {
		t.Fatalf("want 1 summary row for release-build with 1 candidate, got %+v", res.Summary)
	}
}

func TestClusterIssues_EpicFilterGivesDetailNotSummary(t *testing.T) {
	model := clusterModel(t)
	subtasks := []Subtask{
		{ID: "1", Title: "multiarch build fix arm64", Status: "pending"},
		{ID: "2", Title: "multiarch build fix amd64", Status: "pending"},
	}
	res := ClusterIssues(model, subtasks, "release-build", 2, testHints())
	if len(res.Summary) != 0 {
		t.Errorf("--epic mode must not populate Summary, got %+v", res.Summary)
	}
	if len(res.Detail) != 1 || res.Detail[0].Count != 2 {
		t.Fatalf("want 1 detail candidate with 2 tasks, got %+v", res.Detail)
	}
	if res.Detail[0].TaskTitles["1"] != "multiarch build fix arm64" {
		t.Errorf("detail mode must carry task titles, got %+v", res.Detail[0].TaskTitles)
	}
}
