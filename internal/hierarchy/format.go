package hierarchy

import (
	"fmt"
	"sort"
	"strings"
)

// FormatShow renders mr as `hierarchy show`'s human-readable report — the full
// 5-level taxonomy: project -> milestones (rollup + state) -> epics (rollup)
// -> issues + legacy nested tasks (rollup). The top fits on one screen.
func FormatShow(mr ModelRollup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project: %s\n", mr.Project)
	for _, ms := range mr.Milestones {
		fmt.Fprintf(&b, "\n%s: %s [%s] — %d/%d terminal%s\n",
			ms.Milestone.ID, ms.Milestone.Title, ms.Milestone.State, ms.Rollup.Terminal(), ms.Rollup.Total, completeMark(ms.Rollup))
		for _, e := range ms.Epics {
			state := e.Epic.State
			if state == "" {
				state = "?"
			}
			fmt.Fprintf(&b, "  %-32s [%-6s] %d/%d terminal%s\n", e.Epic.ID, state, e.Rollup.Terminal(), e.Rollup.Total, completeMark(e.Rollup))
			for _, tk := range e.Tasks {
				fmt.Fprintf(&b, "    %-30s %d/%d terminal%s\n", tk.Task.ID, tk.Rollup.Terminal(), tk.Rollup.Total, completeMark(tk.Rollup))
			}
			for _, is := range e.Issues {
				fmt.Fprintf(&b, "    issue %-6s %-26s %d/%d terminal%s\n", is.Issue.ID, issueTitleOrPlaceholder(is.Issue), is.Rollup.Terminal(), is.Rollup.Total, completeMark(is.Rollup))
			}
		}
	}
	fmt.Fprintf(&b, "\nunmapped tasks: %d\n", len(mr.Unmapped))
	fmt.Fprintf(&b, "unmapped issues: %d\n", len(mr.UnmappedIssues))
	return b.String()
}

// FormatTree is FormatShow scoped to one milestone and/or epic — `leadkit tree
// [--milestone X] [--epic Y]`. An empty filter matches everything at that level.
func FormatTree(mr ModelRollup, milestoneFilter, epicFilter string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project: %s\n", mr.Project)
	for _, ms := range mr.Milestones {
		if milestoneFilter != "" && ms.Milestone.ID != milestoneFilter {
			continue
		}
		fmt.Fprintf(&b, "\n%s: %s [%s] — %d/%d terminal%s\n",
			ms.Milestone.ID, ms.Milestone.Title, ms.Milestone.State, ms.Rollup.Terminal(), ms.Rollup.Total, completeMark(ms.Rollup))
		for _, e := range ms.Epics {
			if epicFilter != "" && e.Epic.ID != epicFilter {
				continue
			}
			state := e.Epic.State
			if state == "" {
				state = "?"
			}
			fmt.Fprintf(&b, "  %-32s [%-6s] %d/%d terminal%s\n", e.Epic.ID, state, e.Rollup.Terminal(), e.Rollup.Total, completeMark(e.Rollup))
			for _, tk := range e.Tasks {
				fmt.Fprintf(&b, "    %-30s %d/%d terminal%s\n", tk.Task.ID, tk.Rollup.Terminal(), tk.Rollup.Total, completeMark(tk.Rollup))
			}
			for _, is := range e.Issues {
				fmt.Fprintf(&b, "    issue %-6s %-26s %d/%d terminal%s\n", is.Issue.ID, issueTitleOrPlaceholder(is.Issue), is.Rollup.Terminal(), is.Rollup.Total, completeMark(is.Rollup))
			}
		}
	}
	return b.String()
}

// FormatNode renders one Node (get's result) human-readably.
func FormatNode(n Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", n.Kind, n.ID)
	if n.Title != "" {
		fmt.Fprintf(&b, "  title:  %s\n", n.Title)
	}
	if n.Status != "" {
		fmt.Fprintf(&b, "  status: %s\n", n.Status)
	}
	if n.Parent != "" {
		fmt.Fprintf(&b, "  parent: %s\n", n.Parent)
	}
	if n.Unmapped {
		b.WriteString("  unmapped: true\n")
	}
	if n.Rollup != nil {
		fmt.Fprintf(&b, "  rollup: %d/%d terminal%s\n", n.Rollup.Terminal(), n.Rollup.Total, completeMark(*n.Rollup))
	}
	if len(n.Children) > 0 {
		fmt.Fprintf(&b, "  children (%d): %s\n", len(n.Children), strings.Join(n.Children, ", "))
	}
	return b.String()
}

// FormatNodeList renders a `list` result — one compact line per node.
func FormatNodeList(nodes []Node) string {
	var b strings.Builder
	for _, n := range nodes {
		status := n.Status
		if status == "" {
			status = "?"
		}
		fmt.Fprintf(&b, "%-8s [%-12s]", n.ID, status)
		if n.Title != "" {
			fmt.Fprintf(&b, " %s", n.Title)
		}
		if n.Rollup != nil {
			fmt.Fprintf(&b, " — %d/%d terminal%s", n.Rollup.Terminal(), n.Rollup.Total, completeMark(*n.Rollup))
		}
		if n.Unmapped {
			b.WriteString(" (unmapped)")
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%d node(s)\n", len(nodes))
	return b.String()
}

// FormatProposeResult renders res as `hierarchy propose`'s human-readable report:
// one line per proposal, then the summary counts.
func FormatProposeResult(res ProposeResult) string {
	var b strings.Builder
	for _, p := range res.Proposals {
		fmt.Fprintf(&b, "%s -> %s [%s] %s\n", p.ID, p.ProposedParent, p.Confidence, p.Why)
	}
	b.WriteString("\nby epic:\n")
	epics := make([]string, 0, len(res.ByEpic))
	for e := range res.ByEpic {
		epics = append(epics, e)
	}
	sort.Strings(epics)
	for _, e := range epics {
		fmt.Fprintf(&b, "  %s: %d\n", e, res.ByEpic[e])
	}
	fmt.Fprintf(&b, "\nsummary: %d proposals (%d high, %d medium, %d low confidence), %d future-work\n",
		len(res.Proposals), res.High, res.Medium, res.Low, res.FutureWork)
	return b.String()
}

func completeMark(r Rollup) string {
	if r.Total == 0 {
		return " (no subtasks mapped yet)"
	}
	if r.Complete {
		return " — COMPLETE"
	}
	return ""
}

func issueTitleOrPlaceholder(is Issue) string {
	if is.Title == "" {
		return "(untitled)"
	}
	return is.Title
}

// FormatClusterResult renders res as `hierarchy cluster-issues`' human-readable
// report — whichever of Summary/Detail is populated (see ClusterResult's own doc
// comment: exactly one of the two is set per call).
func FormatClusterResult(res ClusterResult) string {
	var b strings.Builder
	if len(res.Detail) > 0 {
		for _, c := range res.Detail {
			fmt.Fprintf(&b, "%s  (%d task%s, %d done / %d active)\n", c.SuggestedName, c.Count, plural(c.Count), c.DoneCount, c.ActiveCount)
			if len(c.SharedTokens) > 0 {
				fmt.Fprintf(&b, "  shared: %s\n", strings.Join(c.SharedTokens, ", "))
			}
			for _, id := range c.TaskIDs {
				fmt.Fprintf(&b, "  %-8s %s\n", id, c.TaskTitles[id])
			}
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d candidate issue(s)\n", len(res.Detail))
		return b.String()
	}
	for _, s := range res.Summary {
		fmt.Fprintf(&b, "%-32s %2d candidate issue(s)  sizes=%v  done=%d active=%d\n", s.Epic, s.Candidates, s.Sizes, s.Done, s.Active)
	}
	fmt.Fprintf(&b, "\n%d epic(s) with proposed tasks; %d task(s) need an epic first (not clustered)\n", len(res.Summary), res.NeedsEpic)
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
