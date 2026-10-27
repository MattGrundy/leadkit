package hierarchy

import "sort"

// Rollup is one node's (issue, epic, nested task, or milestone) child-status
// breakdown.
type Rollup struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
	Complete bool           `json:"complete"` // true iff Total > 0 AND every child is terminal
}

func newRollup() Rollup { return Rollup{ByStatus: map[string]int{}} }

func (r *Rollup) add(status string) {
	r.Total++
	r.ByStatus[normalizeStatus(status)]++
}

// Terminal returns how many of this rollup's children are in a terminal status.
func (r Rollup) Terminal() int {
	n := 0
	for s, c := range r.ByStatus {
		if terminalStatuses[s] {
			n += c
		}
	}
	return n
}

func (r *Rollup) finalize() {
	r.Complete = r.Total > 0 && r.Terminal() == r.Total
}

func mergeRollup(a, b Rollup) Rollup {
	out := newRollup()
	out.Total = a.Total + b.Total
	for s, c := range a.ByStatus {
		out.ByStatus[s] += c
	}
	for s, c := range b.ByStatus {
		out.ByStatus[s] += c
	}
	return out
}

// TaskRollup pairs a nested Task (an epic's own optional LEGACY breakdown item in
// milestones.yaml, e.g. quarantine's "allure"/"quarantine-heuristic"/
// "quarantine-reporting" — pre-dating the real Issue file level) with its rollup.
type TaskRollup struct {
	Task   Task   `json:"task"`
	Rollup Rollup `json:"rollup"`
}

// IssueRollup pairs a real Issue (issues/*.md) with its rollup — merged from every
// Task (tasks/*.md) whose `parent:` names this issue.
type IssueRollup struct {
	Issue  Issue  `json:"issue"`
	Rollup Rollup `json:"rollup"`
}

// EpicRollup pairs an Epic with its rollup — Rollup here is the epic's OWN direct
// tasks (legacy, parented straight to the epic) merged with every nested legacy
// Task's rollup AND every real Issue's rollup (a nested task or an issue is a
// sub-grouping WITHIN the epic, not a sibling at the milestone level).
type EpicRollup struct {
	Epic   Epic          `json:"epic"`
	Tasks  []TaskRollup  `json:"tasks,omitempty"`
	Issues []IssueRollup `json:"issues,omitempty"`
	Rollup Rollup        `json:"rollup"`
}

// MilestoneRollup pairs a Milestone with its rollup (merged across all its epics).
type MilestoneRollup struct {
	Milestone Milestone    `json:"milestone"`
	Epics     []EpicRollup `json:"epics"`
	Rollup    Rollup       `json:"rollup"`
}

// ModelRollup is `hierarchy show`'s full computed tree — the taxonomy's Project
// level down through Milestone/Epic/Issue, with Task rolled up into whichever Issue
// (or, legacy, epic/nested-task) it's parented to.
type ModelRollup struct {
	Project    string            `json:"project"`
	Milestones []MilestoneRollup `json:"milestones"`
	// Unmapped is every Task id with no parent:, or a parent: unresolvable against
	// this model (no epic/nested-task/issue matches it).
	Unmapped []string `json:"unmapped"`
	// UnmappedIssues is every Issue with no parent:, or a parent: that isn't a known
	// epic id — WITH its own rollup (still merged from its own child Tasks, even
	// though it isn't attached anywhere in Milestones above), not just a bare id, so
	// `get`/`list` can still answer for it.
	UnmappedIssues []IssueRollup `json:"unmapped_issues"`
}

// ComputeRollup builds the full 5-level rollup tree from model + every issue's
// `parent:` (epic id) + every task's `parent:` (issue id, or — legacy — an epic/
// nested-task id directly). A subtask whose Parent is empty, OR set but unresolvable,
// is Unmapped; same for an issue whose Parent isn't a known epic — dangling parents
// are surfaced here rather than silently dropped or guessed at.
func ComputeRollup(model Model, issues []Issue, subtasks []Subtask) ModelRollup {
	epicIDs := model.EpicIDs()
	known := model.KnownIDs() // epic ids + legacy nested-task ids
	for _, is := range issues {
		known[is.ID] = true // an issue is now also a valid Task parent target
	}

	byParent := map[string][]Subtask{}
	var unmappedTasks []string
	for _, s := range subtasks {
		if s.Parent == "" || !known[s.Parent] {
			unmappedTasks = append(unmappedTasks, s.ID)
			continue
		}
		byParent[s.Parent] = append(byParent[s.Parent], s)
	}
	sort.Strings(unmappedTasks)

	issuesByEpic := map[string][]Issue{}
	var unmappedIssues []IssueRollup
	for _, is := range issues {
		if is.Parent == "" || !epicIDs[is.Parent] {
			r := newRollup()
			for _, s := range byParent[is.ID] {
				r.add(s.Status)
			}
			r.finalize()
			unmappedIssues = append(unmappedIssues, IssueRollup{Issue: is, Rollup: r})
			continue
		}
		issuesByEpic[is.Parent] = append(issuesByEpic[is.Parent], is)
	}
	sort.Slice(unmappedIssues, func(i, j int) bool { return unmappedIssues[i].Issue.ID < unmappedIssues[j].Issue.ID })
	for epicID := range issuesByEpic {
		list := issuesByEpic[epicID]
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		issuesByEpic[epicID] = list
	}

	var milestones []MilestoneRollup
	for _, ms := range model.Milestones {
		var epicRollups []EpicRollup
		msRoll := newRollup()
		for _, e := range ms.Epics {
			epicRoll := newRollup()
			for _, s := range byParent[e.ID] {
				epicRoll.add(s.Status)
			}
			var taskRolls []TaskRollup
			for _, tk := range e.Tasks {
				tRoll := newRollup()
				for _, s := range byParent[tk.ID] {
					tRoll.add(s.Status)
				}
				tRoll.finalize()
				taskRolls = append(taskRolls, TaskRollup{Task: tk, Rollup: tRoll})
				epicRoll = mergeRollup(epicRoll, tRoll)
			}
			var issueRolls []IssueRollup
			for _, is := range issuesByEpic[e.ID] {
				iRoll := newRollup()
				for _, s := range byParent[is.ID] {
					iRoll.add(s.Status)
				}
				iRoll.finalize()
				issueRolls = append(issueRolls, IssueRollup{Issue: is, Rollup: iRoll})
				epicRoll = mergeRollup(epicRoll, iRoll)
			}
			epicRoll.finalize()
			epicRollups = append(epicRollups, EpicRollup{Epic: e, Tasks: taskRolls, Issues: issueRolls, Rollup: epicRoll})
			msRoll = mergeRollup(msRoll, epicRoll)
		}
		msRoll.finalize()
		milestones = append(milestones, MilestoneRollup{Milestone: ms, Epics: epicRollups, Rollup: msRoll})
	}

	return ModelRollup{Project: model.Project, Milestones: milestones, Unmapped: unmappedTasks, UnmappedIssues: unmappedIssues}
}
