// Package sync is the pluggable output-sync layer (docs/HIERARCHY-AND-SYNC.md):
// local is canonical, trackers (Linear, GitHub, ...) are one-way projections. leadkit
// COMPUTES the desired tracker state (BuildSyncModel + Output.Plan — pure,
// read-only, testable); by default an AGENT with the tracker's own MCP EXECUTES
// each Op and leadkit holds no tracker token. Linear alone has an opt-in DIRECT
// executor (direct.go — GraphQL API, runtime-only key; see internal/linearapi).
// Plan is identical either way. GitHub is the other shape: it executes IN PROCESS
// through the already-authenticated `gh` CLI (github.go, github_exec.go) — no agent
// half, no token anywhere in leadkit — and mirrors the flat ticket store, not the
// hierarchy. See RecordID for the only local write this
// package performs (writing a created node's
// tracker id back into local frontmatter, so the next Plan sees it as an update).
package sync

import (
	"strings"

	"mgpp.dev/leadkit/internal/hierarchy"
)

// Node is one canonical sync-tree entry — Project/Milestone/Epic/Issue/Task, each
// carrying its own trackers map (the frontmatter `trackers:` block — the
// sync-state itself). An Output's Plan diffs against THIS, never against the
// tracker's own current state, so a Plan never needs to read the tracker back.
type Node struct {
	Level         string            `json:"level"` // "project" | "milestone" | "epic" | "issue" | "task"
	LocalID       string            `json:"local_id"`
	Title         string            `json:"title"`
	Status        string            `json:"status,omitempty"`
	Terminal      bool              `json:"terminal"`
	ParentLocalID string            `json:"parent_local_id,omitempty"`
	Type          string            `json:"type,omitempty"`     // ticket type (developer, qa-agent, ...); only BuildTicketModel fills it
	Priority      string            `json:"priority,omitempty"` // ticket priority (P0..P3); only BuildTicketModel fills it
	Trackers      map[string]string `json:"trackers,omitempty"`
}

// Model is the whole canonical tree, PARENT-FIRST (a Milestone always precedes
// its Epics, an Epic always precedes its Issues, an Issue always precedes its
// Tasks) — exactly the order an Output.Plan needs to create parent-before-child,
// with no separate sort/topological step required.
type Model struct {
	Nodes []Node `json:"nodes"`
}

// projectLocalID is the single synthetic root node's id — there is exactly one
// Project per store, and milestones.yaml carries no explicit id for it.
const projectLocalID = "project"

// terminalStatuses/normalizeStatus mirror internal/hierarchy's own copies —
// duplicated on purpose, this codebase's small-package decoupling convention
// (see e.g. internal/hierarchyapply's identical copy).
var terminalStatuses = map[string]bool{
	"completed": true, "done": true, "dropped": true, "superseded": true,
}

func normalizeStatus(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// BuildSyncModel flattens mr (an already-computed hierarchy rollup — see
// hierarchy.ComputeRollup) + every subtask into the canonical sync tree.
// UNMAPPED issues/tasks (mr.UnmappedIssues, mr.Unmapped) are excluded: they have
// no resolvable parent in the local tree, so there's nothing to project them
// under — exactly why `hierarchy apply` runs first (sync only sees a reviewed,
// mapped tree).
//
// A subtask with no Parent is excluded as well (the `s.Parent == ""` skip): the
// hierarchy tree has no Issue to put it under. The live store is entirely parentless,
// so a tracker that must carry it takes BuildTicketModel instead (see BuildModelFor).
// Linear does not, which is ticket 193's remaining defect.
func BuildSyncModel(mr hierarchy.ModelRollup, subtasks []hierarchy.Subtask) Model {
	subtasksByIssue := map[string][]hierarchy.Subtask{}
	for _, s := range subtasks {
		if s.Parent == "" {
			continue
		}
		subtasksByIssue[s.Parent] = append(subtasksByIssue[s.Parent], s)
	}

	var nodes []Node
	nodes = append(nodes, Node{Level: "project", LocalID: projectLocalID, Title: mr.Project})

	for _, ms := range mr.Milestones {
		nodes = append(nodes, Node{
			Level: "milestone", LocalID: ms.Milestone.ID, Title: ms.Milestone.Title, Status: ms.Milestone.State,
			Terminal: ms.Rollup.Complete, ParentLocalID: projectLocalID,
		})
		for _, e := range ms.Epics {
			nodes = append(nodes, Node{
				Level: "epic", LocalID: e.Epic.ID, Title: e.Epic.ID, Status: e.Epic.State,
				Terminal: e.Rollup.Complete, ParentLocalID: ms.Milestone.ID,
			})
			for _, ir := range e.Issues {
				nodes = append(nodes, Node{
					Level: "issue", LocalID: ir.Issue.ID, Title: ir.Issue.Title, Status: ir.Issue.Status,
					Terminal: terminalStatuses[normalizeStatus(ir.Issue.Status)], ParentLocalID: e.Epic.ID,
					Trackers: ir.Issue.Trackers,
				})
				for _, s := range subtasksByIssue[ir.Issue.ID] {
					nodes = append(nodes, Node{
						Level: "task", LocalID: s.ID, Title: s.Title, Status: s.Status,
						Terminal: terminalStatuses[normalizeStatus(s.Status)], ParentLocalID: ir.Issue.ID,
						Trackers: s.Trackers,
					})
				}
			}
		}
	}
	return Model{Nodes: nodes}
}
