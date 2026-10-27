// Hierarchy/tree view over the same read-only data the board already scans, plus
// milestones.yaml + issues/ — reusing internal/hierarchy's own readers (the same
// ones cmd/leadkit's loadHierarchyModel/loadHierarchyData call) rather than
// reinventing the parse. Nothing here writes anything.
package viewer

import (
	"path/filepath"
	"sort"

	"mgpp.dev/leadkit/internal/hierarchy"
)

// treeTask is one Task leaf row in the tree — an Issue's (or orphan bucket's)
// child, carrying the SAME queue lane/status the kanban board renders (joined in
// by task id), not just the frontmatter status hierarchy.Subtask carries.
type treeTask struct {
	ID, Title, Lane, Status string
}

// treeCounts is a node's rollup — total tasks underneath it, split done/active
// (isDone mirrors the board's own done/not-done split, so the two views agree).
type treeCounts struct{ Total, Done, Active int }

func (c *treeCounts) add(status string) {
	c.Total++
	if isDone(status) {
		c.Done++
	} else {
		c.Active++
	}
}

func mergeCounts(a, b treeCounts) treeCounts {
	return treeCounts{Total: a.Total + b.Total, Done: a.Done + b.Done, Active: a.Active + b.Active}
}

type treeIssue struct {
	ID, Title string
	Tasks     []treeTask
	Counts    treeCounts
}

type treeEpic struct {
	ID, Note, Desc string
	// DirectTasks are tasks whose parent: names this epic directly (the legacy
	// pre-Issue-level shape) — kept visible under the epic rather than mislabelled
	// orphan, since their parent link is in fact valid.
	DirectTasks []treeTask
	Issues      []treeIssue
	Counts      treeCounts
}

type treeMilestone struct {
	ID, Title, State string
	Epics            []treeEpic
	Counts           treeCounts
}

// TreeData is /tree's whole render model.
type TreeData struct {
	Root       string
	Project    string
	Milestones []treeMilestone
	// OrphanIssues is every Issue whose parent: is empty or names no known epic —
	// still shown (with its own tasks), never silently dropped.
	OrphanIssues []treeIssue
	// OrphanTasks is every Task whose parent: is empty or names neither a known
	// Issue nor a known Epic — the tree's own "nothing vanishes" bucket.
	OrphanTasks  []treeTask
	Counts       treeCounts
	HasHierarchy bool // milestones.yaml and/or issues/ produced at least one node
}

// laneStatus is one queue-scanned task's board placement, keyed by task id.
type laneStatus struct{ Lane, Status string }

// queueLaneStatus scans queueRoot (the board's own Scan) and indexes it by task
// id, so tree rows can show "where is this in the workflow" the same way the
// board does. A task not currently placed in the queue overlay (or queueRoot ==
// "") just gets no entry — BuildTree falls back to the store's frontmatter status.
func queueLaneStatus(queueRoot string) map[string]laneStatus {
	out := map[string]laneStatus{}
	if queueRoot == "" {
		return out
	}
	tasks, _ := Scan(queueRoot)
	for _, t := range tasks {
		id := t.Meta["id"]
		if id == "" {
			id = idFromFilename(filepath.Base(t.Rel))
		}
		if id == "" {
			continue
		}
		out[id] = laneStatus{Lane: t.Lane, Status: t.Status}
	}
	return out
}

// idFromFilename extracts a store task's leading "NNN-" id from its filename —
// used only when frontmatter carries no explicit id: field. Duplicated (rather
// than imported) from internal/hierarchy's own copy — this codebase's established
// small-package decoupling convention (see that copy's doc comment).
func idFromFilename(name string) string {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(name) || (name[i] != '-' && name[i] != '.') {
		return ""
	}
	return name[:i]
}

func displayTitle(title, id string) string {
	if title != "" {
		return title
	}
	return id
}

// BuildTree reads milestones.yaml (under storeRoot) + issuesRoot + storeRoot's own
// tasks, and joins in queueRoot's live lane/status, to build the whole collapsible
// Milestone->Epic->Issue->Task tree. Every read is best-effort: a missing/empty
// milestones.yaml or issuesRoot is not an error (hierarchy.Parse/LoadIssues' own
// "not seeded yet" case) — BuildTree just returns a tree with HasHierarchy=false
// rather than crashing, so a project with no hierarchy still gets a (empty) /tree.
func BuildTree(storeRoot, issuesRoot, queueRoot string) TreeData {
	data := TreeData{Root: storeRoot}
	if storeRoot == "" {
		return data
	}

	model, _, _ := hierarchy.Parse(filepath.Join(storeRoot, "milestones.yaml"))
	data.Project = model.Project

	var issues []hierarchy.Issue
	if issuesRoot != "" {
		issues, _ = hierarchy.LoadIssues(issuesRoot)
	}
	subtasks, _ := hierarchy.LoadSubtasks(storeRoot)

	lanes := queueLaneStatus(queueRoot)

	epicIDs := model.EpicIDs()
	known := model.KnownIDs() // epic ids + legacy nested-task ids
	for _, is := range issues {
		known[is.ID] = true // an issue is also a valid task parent target
	}

	toTreeTask := func(s hierarchy.Subtask) treeTask {
		tt := treeTask{ID: s.ID, Title: displayTitle(s.Title, s.ID)}
		if ls, ok := lanes[s.ID]; ok {
			tt.Lane, tt.Status = ls.Lane, ls.Status
		} else {
			tt.Lane, tt.Status = "—", s.Status
		}
		return tt
	}

	tasksByParent := map[string][]treeTask{}
	for _, s := range subtasks {
		tt := toTreeTask(s)
		if s.Parent == "" || !known[s.Parent] {
			data.OrphanTasks = append(data.OrphanTasks, tt)
			continue
		}
		tasksByParent[s.Parent] = append(tasksByParent[s.Parent], tt)
	}
	sortTasks(data.OrphanTasks)
	for k, v := range tasksByParent {
		sortTasks(v)
		tasksByParent[k] = v
	}

	buildIssue := func(is hierarchy.Issue) treeIssue {
		ti := treeIssue{ID: is.ID, Title: displayTitle(is.Title, is.ID), Tasks: tasksByParent[is.ID]}
		for _, t := range ti.Tasks {
			ti.Counts.add(t.Status)
		}
		return ti
	}

	issuesByEpic := map[string][]treeIssue{}
	for _, is := range issues {
		ti := buildIssue(is)
		if is.Parent == "" || !epicIDs[is.Parent] {
			data.OrphanIssues = append(data.OrphanIssues, ti)
			continue
		}
		issuesByEpic[is.Parent] = append(issuesByEpic[is.Parent], ti)
	}
	sort.Slice(data.OrphanIssues, func(i, j int) bool { return data.OrphanIssues[i].ID < data.OrphanIssues[j].ID })
	for k, v := range issuesByEpic {
		sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
		issuesByEpic[k] = v
	}

	for _, ms := range model.Milestones {
		tm := treeMilestone{ID: ms.ID, Title: ms.Title, State: ms.State}
		for _, e := range ms.Epics {
			te := treeEpic{ID: e.ID, Note: e.Note, Desc: e.Desc, Issues: issuesByEpic[e.ID], DirectTasks: tasksByParent[e.ID]}
			for _, t := range te.DirectTasks {
				te.Counts.add(t.Status)
			}
			for _, ti := range te.Issues {
				te.Counts = mergeCounts(te.Counts, ti.Counts)
			}
			tm.Epics = append(tm.Epics, te)
			tm.Counts = mergeCounts(tm.Counts, te.Counts)
		}
		data.Milestones = append(data.Milestones, tm)
		data.Counts = mergeCounts(data.Counts, tm.Counts)
	}
	for _, oi := range data.OrphanIssues {
		data.Counts = mergeCounts(data.Counts, oi.Counts)
	}
	for _, ot := range data.OrphanTasks {
		data.Counts.add(ot.Status)
	}

	data.HasHierarchy = len(data.Milestones) > 0 || len(issues) > 0
	return data
}

func sortTasks(ts []treeTask) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Title < ts[j].Title })
}
