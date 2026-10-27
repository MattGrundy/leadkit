package hierarchy

import "testing"

// testHints is the fixture curated hint table these tests drive Propose with — the
// adopter-supplied seeds that used to be baked into the source (now empty by default).
// Owning the fixture here keeps leadkit source project-agnostic while the tests still
// exercise the type/keyword-hint paths.
func testHints() Hints {
	return Hints{
		Type: map[string][]string{
			"frontend-agent":    {"dev-portal"},
			"ci-pipeline-agent": {"release-build", "scheduled-build"},
			"ci-infra-agent":    {"remove-self-hosted-runners", "remap-legacy-resources-and-spend", "argo-all-prs-dark", "fix-18883-merge-issues"},
			"ci-dx-agent":       {"open-to-dev", "dev-portal"},
			"qa-agent":          {"quarantine"},
		},
		Keyword: []KeywordHint{
			{Keywords: []string{"multiarch"}, Epic: "release-build"},
			{Keywords: []string{"quarantine", "allure", "flake", "flaky"}, Epic: "quarantine"},
			{Keywords: []string{"nexus", "self-hosted", "legacy-build"}, Epic: "remove-legacy-build"},
			{Keywords: []string{"warp", "demo container"}, Epic: FutureWorkEpic},
		},
	}
}

func proposeModel(t *testing.T) Model {
	t.Helper()
	return Model{
		Milestones: []Milestone{
			{
				ID: "shadow-ci",
				Epics: []Epic{
					{ID: "dev-portal"},
					{ID: "release-build"},
					{ID: "quarantine"},
					{ID: "remove-legacy-build"},
				},
			},
			{ID: "future-work", Epics: []Epic{{ID: FutureWorkEpic}}},
		},
	}
}

func TestPropose_FrontendTypeSubtaskProposesDevPortal(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{{ID: "1", Type: "frontend-agent", Title: "some UI work", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if len(res.Proposals) != 1 || res.Proposals[0].ProposedParent != "dev-portal" {
		t.Fatalf("want dev-portal, got %+v", res.Proposals)
	}
	if res.Proposals[0].Confidence != ConfidenceMedium {
		t.Errorf("Confidence = %q, want medium (type-only signal)", res.Proposals[0].Confidence)
	}
}

func TestPropose_MultiarchSubjectProposesReleaseBuild(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{{ID: "1", Title: "Fix multiarch build for arm64", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if res.Proposals[0].ProposedParent != "release-build" {
		t.Fatalf("want release-build, got %+v", res.Proposals[0])
	}
	if res.Proposals[0].Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want high (keyword signal)", res.Proposals[0].Confidence)
	}
}

func TestPropose_NonMigrationSubjectProposesFutureWork(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{{ID: "1", Title: "Set up warp.dev for demo container access", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if res.Proposals[0].ProposedParent != FutureWorkEpic {
		t.Fatalf("want future-work, got %+v", res.Proposals[0])
	}
}

func TestPropose_TrulyUnrelatedSubjectAlsoProposesFutureWork(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{{ID: "500", Title: "Completely unrelated one-off thing", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if res.Proposals[0].ProposedParent != FutureWorkEpic {
		t.Fatalf("want future-work for a no-signal subtask, got %+v", res.Proposals[0])
	}
	if res.FutureWork != 1 {
		t.Errorf("FutureWork count = %d, want 1", res.FutureWork)
	}
}

func TestPropose_ActiveOnlySkipsTerminalStatus(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{
		{ID: "1", Title: "multiarch fix", Status: "completed"},
		{ID: "2", Title: "multiarch fix 2", Status: "pending"},
	}
	res := Propose(model, subtasks, true, testHints())
	if len(res.Proposals) != 1 || res.Proposals[0].ID != "2" {
		t.Fatalf("--active-only should skip the completed task 1, got %+v", res.Proposals)
	}
}

func TestPropose_DefaultIncludesTerminalStatus(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{
		{ID: "1", Title: "multiarch fix", Status: "completed"},
		{ID: "2", Title: "multiarch fix 2", Status: "pending"},
	}
	res := Propose(model, subtasks, false, testHints())
	if len(res.Proposals) != 2 {
		t.Fatalf("default (no --active-only) should propose for ALL subtasks incl. terminal, got %+v", res.Proposals)
	}
}

func TestPropose_AlreadyMappedSubtaskIsSkipped(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{{ID: "1", Title: "multiarch fix", Status: "pending", Parent: "dev-portal"}}
	res := Propose(model, subtasks, false, testHints())
	if len(res.Proposals) != 0 {
		t.Errorf("an already-mapped subtask must not get a proposal, got %+v", res.Proposals)
	}
}

func TestPropose_NoSignalNeverInheritsFromANeighbor(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{
		{ID: "10", Title: "multiarch build fix", Status: "pending"},           // keyword -> release-build (signaled)
		{ID: "11", Title: "totally generic followup work", Status: "pending"}, // no signal on its own
	}
	res := Propose(model, subtasks, false, testHints())
	byID := map[string]Proposal{}
	for _, p := range res.Proposals {
		byID[p.ID] = p
	}
	if byID["11"].ProposedParent != FutureWorkEpic {
		t.Errorf("a signal-less task must NOT be gap-filled from an adjacent-id neighbor (that cascade is forbidden by design), got %+v", byID["11"])
	}
}

func TestPropose_AmbiguousTypeMatchTiebreaksFromAdjacentSignaledNeighbor(t *testing.T) {
	model := Model{Milestones: []Milestone{{ID: "m", Epics: []Epic{
		{ID: "remove-self-hosted-runners"}, {ID: "remap-legacy-resources-and-spend"},
		{ID: "argo-all-prs-dark"}, {ID: "fix-18883-merge-issues"},
	}}}}
	subtasks := []Subtask{
		// keyword -> remove-self-hosted-runners, HIGH, independently signaled.
		{ID: "10", Title: "decommission self-hosted runner pool", Type: "ci-infra-agent", Status: "pending"},
		// ci-infra-agent alone is ambiguous across 4 epics — no keyword to disambiguate.
		{ID: "11", Title: "generic ci-infra followup", Type: "ci-infra-agent", Status: "pending"},
	}
	res := Propose(model, subtasks, false, testHints())
	byID := map[string]Proposal{}
	for _, p := range res.Proposals {
		byID[p.ID] = p
	}
	if byID["11"].ProposedParent != "remove-self-hosted-runners" {
		t.Errorf("an ambiguous type match should tiebreak toward the candidate its adjacent-id neighbor independently signaled, got %+v", byID["11"])
	}
	if byID["11"].Confidence != ConfidenceMedium {
		t.Errorf("a tiebreak resolution should read medium confidence, got %q", byID["11"].Confidence)
	}
}

func TestPropose_TiebreakRespectsWindowBoundAndCandidateSet(t *testing.T) {
	model := Model{Milestones: []Milestone{{ID: "m", Epics: []Epic{
		{ID: "remove-self-hosted-runners"}, {ID: "remap-legacy-resources-and-spend"},
		{ID: "argo-all-prs-dark"}, {ID: "fix-18883-merge-issues"}, {ID: "quarantine"},
	}}}}
	subtasks := []Subtask{
		{ID: "10", Title: "flaky quarantine test", Status: "pending"},                               // keyword -> quarantine, not a candidate for id 9999, also out of window
		{ID: "9999", Title: "generic ci-infra followup", Type: "ci-infra-agent", Status: "pending"}, // ambiguous, no in-window/in-candidate-set neighbor
	}
	res := Propose(model, subtasks, false, testHints())
	byID := map[string]Proposal{}
	for _, p := range res.Proposals {
		byID[p.ID] = p
	}
	if byID["9999"].ProposedParent == "quarantine" {
		t.Errorf("tiebreak must never adopt a neighbor's epic that isn't one of this task's own candidates, got %+v", byID["9999"])
	}
	if byID["9999"].Confidence != ConfidenceLow {
		t.Errorf("an unresolved ambiguous match (no in-window in-candidate-set neighbor) stays low confidence, got %q", byID["9999"].Confidence)
	}
}

func TestPropose_ByEpicSummaryCounts(t *testing.T) {
	model := proposeModel(t)
	subtasks := []Subtask{
		{ID: "1", Title: "multiarch fix", Status: "pending"},
		{ID: "2", Title: "another multiarch thing", Status: "pending"},
		{ID: "500", Title: "random unrelated", Status: "pending"}, // no signal at all — must stay future-work
	}
	res := Propose(model, subtasks, false, testHints())
	if res.ByEpic["release-build"] != 2 {
		t.Errorf("ByEpic[release-build] = %d, want 2", res.ByEpic["release-build"])
	}
	if res.ByEpic[FutureWorkEpic] != 1 {
		t.Errorf("ByEpic[future-work] = %d, want 1", res.ByEpic[FutureWorkEpic])
	}
}

func TestMatchKeyword_EpicAliasClaimsTaskIDAloneWouldMiss(t *testing.T) {
	model := Model{Milestones: []Milestone{{ID: "m", Epics: []Epic{
		{ID: "spike-20608", Aliases: []string{"20608", "18883"}},
	}}}}
	subtasks := []Subtask{{ID: "1", Title: "PR #18883 history rewrite", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if res.Proposals[0].ProposedParent != "spike-20608" {
		t.Fatalf("want spike-20608 claimed via alias 18883 (id alone would never match), got %+v", res.Proposals[0])
	}
	if res.Proposals[0].Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want high (alias match)", res.Proposals[0].Confidence)
	}
}

func TestMatchKeyword_AliasMatchIsCaseAndSeparatorInsensitive(t *testing.T) {
	model := Model{Milestones: []Milestone{{ID: "m", Epics: []Epic{
		{ID: "open-to-dev", Aliases: []string{"ARGO_PR_CHECK"}},
	}}}}
	subtasks := []Subtask{{ID: "1", Title: "wire up argo pr check flow end to end", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if res.Proposals[0].ProposedParent != "open-to-dev" {
		t.Fatalf("want open-to-dev claimed via case/separator-insensitive alias match, got %+v", res.Proposals[0])
	}
}

// TestPropose_EmptyHintsUsesGenericMatchOnly is the project-agnostic default
// (docs/build-tasks/project-agnostic-defaults.md §3): with a ZERO Hints (no
// leadkit.yaml hierarchy block — the shipped default), Propose bakes in NO adopter
// keyword/type seeds, so a title that only a curated keyword hint would have caught
// falls through to future-work, while the GENERIC model-driven alias match still fires.
func TestPropose_EmptyHintsUsesGenericMatchOnly(t *testing.T) {
	model := Model{Milestones: []Milestone{{ID: "m", Epics: []Epic{
		{ID: "release-build"},
		{ID: "spike-1", Aliases: []string{"18883"}},
	}}}}
	subtasks := []Subtask{
		// "multiarch" was a baked keyword hint → now, with no hints, no signal.
		{ID: "1", Title: "fix multiarch build", Status: "pending"},
		// The generic alias match still un-strands via the epic's own alias.
		{ID: "2", Title: "PR #18883 rewrite", Status: "pending"},
	}
	res := Propose(model, subtasks, false, Hints{}) // ZERO hints — the shipped default
	byID := map[string]Proposal{}
	for _, p := range res.Proposals {
		byID[p.ID] = p
	}
	if byID["1"].ProposedParent != FutureWorkEpic {
		t.Errorf("with no configured hints, a curated-keyword-only title must fall to future-work, got %+v", byID["1"])
	}
	if byID["2"].ProposedParent != "spike-1" {
		t.Errorf("the generic alias match must still fire with empty hints, got %+v", byID["2"])
	}
}

func TestMatchKeyword_AbsentAliasesIsTodaysBehavior(t *testing.T) {
	model := proposeModel(t) // none of these epics carry aliases
	subtasks := []Subtask{{ID: "1", Title: "a completely generic task with no special words", Status: "pending"}}
	res := Propose(model, subtasks, false, testHints())
	if res.Proposals[0].ProposedParent != FutureWorkEpic {
		t.Fatalf("with no aliases anywhere, an unrelated title must still fall through to future-work, got %+v", res.Proposals[0])
	}
}
