package hierarchy

import "strings"

// Node is one taxonomy item (Milestone/Epic/Issue/Task), tagged with its ancestry —
// the uniform shape `get`, `list`, and `tree` all resolve against, so the lead/
// orchestrator have exactly ONE interface instead of ad-hoc grep over the store.
type Node struct {
	Kind        string   `json:"kind"` // "milestone" | "epic" | "issue" | "task"
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Status      string   `json:"status,omitempty"` // milestone/epic: State; issue/task: Status
	Parent      string   `json:"parent,omitempty"`
	MilestoneID string   `json:"milestone_id,omitempty"`
	EpicID      string   `json:"epic_id,omitempty"`
	IssueID     string   `json:"issue_id,omitempty"`
	Rollup      *Rollup  `json:"rollup,omitempty"`   // nil for a Task (leaf — no children)
	Children    []string `json:"children,omitempty"` // direct child ids
	Unmapped    bool     `json:"unmapped,omitempty"` // issue/task only: no parent, or parent unresolvable
}

// AllNodes flattens mr (an already-computed rollup, so its numbers exactly match
// `hierarchy show`'s) + every issue/subtask into one uniform list — Get and List's
// shared foundation, so both read exactly the same shape from exactly one pass.
func AllNodes(mr ModelRollup, subtasks []Subtask) []Node {
	milestoneOfEpic := map[string]string{}
	epicOfIssue := map[string]string{}
	epicOfNestedTask := map[string]string{}

	var out []Node
	for _, ms := range mr.Milestones {
		var msChildren []string
		for _, e := range ms.Epics {
			msChildren = append(msChildren, e.Epic.ID)
			milestoneOfEpic[e.Epic.ID] = ms.Milestone.ID
		}
		msRoll := ms.Rollup
		out = append(out, Node{Kind: "milestone", ID: ms.Milestone.ID, Title: ms.Milestone.Title, Status: ms.Milestone.State, Rollup: &msRoll, Children: msChildren})

		for _, e := range ms.Epics {
			var eChildren []string
			for _, is := range e.Issues {
				eChildren = append(eChildren, is.Issue.ID)
				epicOfIssue[is.Issue.ID] = e.Epic.ID
			}
			for _, tk := range e.Tasks {
				eChildren = append(eChildren, tk.Task.ID)
				epicOfNestedTask[tk.Task.ID] = e.Epic.ID
			}
			eRoll := e.Rollup
			out = append(out, Node{Kind: "epic", ID: e.Epic.ID, Title: e.Epic.ID, Status: e.Epic.State, Parent: ms.Milestone.ID, MilestoneID: ms.Milestone.ID, Rollup: &eRoll, Children: eChildren})

			for _, is := range e.Issues {
				out = append(out, issueNode(is, ms.Milestone.ID, e.Epic.ID, subtasks, false))
			}
		}
	}
	for _, ir := range mr.UnmappedIssues {
		out = append(out, issueNode(ir, "", "", subtasks, true))
	}

	for _, s := range subtasks {
		n := Node{Kind: "task", ID: s.ID, Title: s.Title, Status: s.Status, Parent: s.Parent}
		switch {
		case s.Parent != "" && epicOfIssue[s.Parent] != "":
			n.IssueID = s.Parent
			n.EpicID = epicOfIssue[s.Parent]
			n.MilestoneID = milestoneOfEpic[n.EpicID]
		case s.Parent != "" && milestoneOfEpic[s.Parent] != "":
			// Parented directly to an epic id — legacy/edge case, bypassing Issue.
			n.EpicID = s.Parent
			n.MilestoneID = milestoneOfEpic[s.Parent]
		case s.Parent != "" && epicOfNestedTask[s.Parent] != "":
			// Parented to an epic's own nested legacy YAML breakdown item.
			n.EpicID = epicOfNestedTask[s.Parent]
			n.MilestoneID = milestoneOfEpic[n.EpicID]
		default:
			n.Unmapped = true
		}
		out = append(out, n)
	}
	return out
}

func issueNode(ir IssueRollup, milestoneID, epicID string, subtasks []Subtask, unmapped bool) Node {
	var children []string
	for _, s := range subtasks {
		if s.Parent == ir.Issue.ID {
			children = append(children, s.ID)
		}
	}
	r := ir.Rollup
	return Node{
		Kind: "issue", ID: ir.Issue.ID, Title: ir.Issue.Title, Status: ir.Issue.Status, Parent: ir.Issue.Parent,
		MilestoneID: milestoneID, EpicID: epicID, Rollup: &r, Children: children, Unmapped: unmapped,
	}
}

// Get resolves id at ANY taxonomy level — a milestone/epic id from milestones.yaml,
// or an issues/ or tasks/ file id.
func Get(mr ModelRollup, subtasks []Subtask, id string) (Node, bool) {
	for _, n := range AllNodes(mr, subtasks) {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// List returns every Node at level ("milestone"|"epic"|"issue"|"task"), optionally
// filtered by milestone id, epic id, and/or a case-insensitive exact match on
// status/state — the exact filter set `leadkit list` exposes. An empty filter value
// means "don't filter on this".
func List(mr ModelRollup, subtasks []Subtask, level, milestoneFilter, epicFilter, statusFilter string) []Node {
	var out []Node
	for _, n := range AllNodes(mr, subtasks) {
		if n.Kind != level {
			continue
		}
		if milestoneFilter != "" && n.MilestoneID != milestoneFilter && !(level == "milestone" && n.ID == milestoneFilter) {
			continue
		}
		if epicFilter != "" && n.EpicID != epicFilter && !(level == "epic" && n.ID == epicFilter) {
			continue
		}
		if statusFilter != "" && !strings.EqualFold(n.Status, statusFilter) {
			continue
		}
		out = append(out, n)
	}
	return out
}
