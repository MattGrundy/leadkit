package sync

import "fmt"

// linearLevelMap is this project's chosen Linear mapping (docs/HIERARCHY-AND-
// SYNC.md): Project->Initiative, Milestone->Project, Epic->Milestone,
// Issue->Issue, Task->Sub-issue.
var linearLevelMap = map[string]string{
	"project": "initiative", "milestone": "project", "epic": "milestone",
	"issue": "issue", "task": "sub-issue",
}

// LinearAdapter is the Output implementation for Linear.
type LinearAdapter struct{}

func (LinearAdapter) Name() string { return "linear" }

// Plan walks model (already parent-first) and, per node, diffs against its OWN
// trackers["linear"]: present means update (or close, if Terminal); absent means
// create, with the parent's linear id resolved from the PARENT node's own
// trackers (nodes appear parent-first, so the parent has already been visited —
// its tracker id may still be empty if it hasn't been created/recorded yet; that
// surfaces as an empty ParentTrackerID, not an error, since the parent's own
// create Op already precedes this one in the plan).
func (LinearAdapter) Plan(model Model) ([]Op, error) {
	byID := make(map[string]Node, len(model.Nodes))
	for _, n := range model.Nodes {
		byID[n.LocalID] = n
	}

	ops := make([]Op, 0, len(model.Nodes))
	for _, n := range model.Nodes {
		level, ok := linearLevelMap[n.Level]
		if !ok {
			return nil, fmt.Errorf("sync: linear adapter has no level mapping for %q", n.Level)
		}
		fields := map[string]string{"title": n.Title}
		if n.Status != "" {
			fields["status"] = n.Status
		}

		if id, has := n.Trackers["linear"]; has && id != "" {
			action := "update"
			if n.Terminal {
				action = "close"
			}
			ops = append(ops, Op{Action: action, Level: level, LocalID: n.LocalID, Fields: fields})
			continue
		}

		var parentTrackerID string
		if n.ParentLocalID != "" {
			if p, ok := byID[n.ParentLocalID]; ok {
				parentTrackerID = p.Trackers["linear"]
			}
		}
		ops = append(ops, Op{Action: "create", Level: level, LocalID: n.LocalID, ParentTrackerID: parentTrackerID, Fields: fields})
	}
	return ops, nil
}
