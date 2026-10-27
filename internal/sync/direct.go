package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"mgpp.dev/leadkit/internal/linearapi"
)

// DirectConfig is the identifiers-only slice of config.LinearConfig the direct
// executor needs (never a credential — that lives only inside the linearapi.Client).
type DirectConfig struct {
	TeamKey        string
	RootInitiative string
	ClosedState    string
	DefaultState   string
}

// DirectResult counts what Execute did.
type DirectResult struct{ Created, Updated, Closed int }

// DirectExecutor applies the SAME []Op `sync plan` emits, but against the Linear
// GraphQL API instead of through the sync-executor agent's MCP. The plan is
// mode-agnostic; only execution differs. Record is `sync record-id`'s write-back
// (syncpkg.RecordID) so both modes converge on the same recorded state.
type DirectExecutor struct {
	C      *linearapi.Client
	Cfg    DirectConfig
	Record func(output, localID, trackerID string) error
}

type page struct {
	HasNext bool   `json:"hasNextPage"`
	End     string `json:"endCursor"`
}

type idNode struct {
	ID string `json:"id"`
}

// Execute runs ops (parent-first, as Plan emitted them) against Linear.
//
// Idempotency: Issue/Task nodes use their recorded tracker id (from model) —
// present means update/close (never recreate), absent means create then Record
// the id IMMEDIATELY. Project/Milestone/Epic have no recordable id
// (milestones.yaml, which record-id never writes), so they are found by NAME
// under their parent before creating — a rename of one of those locally will
// therefore create a new Linear node (known limitation).
func (e *DirectExecutor) Execute(ctx context.Context, model Model, ops []Op) (DirectResult, error) {
	var res DirectResult
	byLocal := make(map[string]Node, len(model.Nodes))
	ids := map[string]string{}
	for _, n := range model.Nodes {
		byLocal[n.LocalID] = n
		if id := n.Trackers["linear"]; id != "" {
			ids[n.LocalID] = id
		}
	}

	teamID, err := e.teamID(ctx)
	if err != nil {
		return res, err
	}
	states, err := e.workflowStates(ctx, teamID)
	if err != nil {
		return res, err
	}
	stateID := func(name string) (string, error) {
		id, ok := states[strings.ToLower(name)]
		if !ok {
			return "", fmt.Errorf("linear: team has no workflow state named %q (check linear.closed_state/default_state)", name)
		}
		return id, nil
	}

	// Phase 1: Project/Milestone/Epic, sequentially, in plan (parent-first) order.
	var issueOps []Op
	for _, op := range ops {
		switch op.Level {
		case "initiative", "project", "milestone":
			created, err := e.ensureStructure(ctx, op, byLocal, ids, teamID)
			if err != nil {
				return res, fmt.Errorf("%s %s: %w", op.Level, op.LocalID, err)
			}
			if created {
				res.Created++
			} else {
				res.Updated++
			}
		default:
			issueOps = append(issueOps, op)
		}
	}

	// Phase 2a: existence check for every recorded id in ONE paginated query —
	// a stale id fails loud instead of being silently recreated.
	var recorded []string
	for _, op := range issueOps {
		if op.Action != "create" {
			recorded = append(recorded, ids[op.LocalID])
		}
	}
	if err := e.verifyIssues(ctx, recorded); err != nil {
		return res, err
	}

	// Phase 2b: updates/closes, batched.
	var batch []linearapi.BatchOp
	flush := func() error {
		_, err := e.C.Batch(ctx, batch)
		batch = nil
		return err
	}
	for _, op := range issueOps {
		if op.Action == "create" {
			continue
		}
		id := ids[op.LocalID]
		if id == "" {
			return res, fmt.Errorf("%s %s: plan says %s but no recorded Linear id", op.Level, op.LocalID, op.Action)
		}
		input := map[string]any{"title": op.Fields["title"]}
		if op.Action == "close" {
			sid, err := stateID(e.Cfg.ClosedState)
			if err != nil {
				return res, err
			}
			input["stateId"] = sid
			res.Closed++
		} else {
			res.Updated++
		}
		batch = append(batch, linearapi.BatchOp{
			Call: "issueUpdate(id: $id#, input: $in#)", Decl: "$id#: String!, $in#: IssueUpdateInput!",
			Vars: map[string]any{"id#": id, "in#": input}, Sel: "success",
		})
		if len(batch) == linearapi.MaxBatch {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}

	// Phase 2c: creates, in waves — every op whose parent id is already known
	// goes into one batched request; ids come back, are recorded at once, and
	// unlock the next wave (issues -> sub-issues).
	var pending []Op
	for _, op := range issueOps {
		if op.Action == "create" {
			pending = append(pending, op)
		}
	}
	defState, err := stateID(e.Cfg.DefaultState)
	if len(pending) > 0 && err != nil {
		return res, err
	}
	for len(pending) > 0 {
		var wave, rest []Op
		for _, op := range pending {
			parent := e.parentID(op, byLocal, ids)
			if len(wave) < linearapi.MaxBatch && (parent != "" || byLocal[op.LocalID].ParentLocalID == "") {
				wave = append(wave, op)
			} else {
				rest = append(rest, op)
			}
		}
		if len(wave) == 0 {
			return res, fmt.Errorf("%s %s: parent has no Linear id yet (parent op missing from the plan?)", rest[0].Level, rest[0].LocalID)
		}
		bops := make([]linearapi.BatchOp, len(wave))
		for i, op := range wave {
			input := map[string]any{"teamId": teamID, "title": op.Fields["title"], "stateId": defState}
			if op.Level == "sub-issue" {
				input["parentId"] = e.parentID(op, byLocal, ids)
			} else if epic, ok := byLocal[byLocal[op.LocalID].ParentLocalID]; ok {
				if mid := ids[epic.LocalID]; mid != "" {
					input["projectMilestoneId"] = mid
				}
				if ms, ok := byLocal[epic.ParentLocalID]; ok && ids[ms.LocalID] != "" {
					input["projectId"] = ids[ms.LocalID]
				}
			}
			bops[i] = linearapi.BatchOp{
				Call: "issueCreate(input: $in#)", Decl: "$in#: IssueCreateInput!",
				Vars: map[string]any{"in#": input}, Sel: "success issue { id }",
			}
		}
		// A batch can fail PARTIALLY: Linear returns data for the aliases that
		// succeeded alongside errors. Record every created id BEFORE surfacing any
		// error (an unrecorded create would be re-created on the next run), and keep
		// recording the rest of the wave if one Record fails.
		results, batchErr := e.C.Batch(ctx, bops)
		errs := []error{}
		if batchErr != nil {
			errs = append(errs, batchErr)
		}
		for i, op := range wave {
			var r struct {
				Issue idNode `json:"issue"`
			}
			if i >= len(results) || json.Unmarshal(results[i], &r) != nil || r.Issue.ID == "" {
				if batchErr == nil {
					errs = append(errs, fmt.Errorf("%s %s: issueCreate returned no id", op.Level, op.LocalID))
				}
				continue
			}
			ids[op.LocalID] = r.Issue.ID
			res.Created++
			if e.Record != nil {
				if err := e.Record("linear", op.LocalID, r.Issue.ID); err != nil {
					errs = append(errs, fmt.Errorf("created %s as %s but record-id failed: %w", op.LocalID, r.Issue.ID, err))
				}
			}
		}
		if len(errs) > 0 {
			return res, errors.Join(errs...)
		}
		pending = rest
	}
	return res, nil
}

func (e *DirectExecutor) parentID(op Op, byLocal map[string]Node, ids map[string]string) string {
	if op.ParentTrackerID != "" {
		return op.ParentTrackerID
	}
	return ids[byLocal[op.LocalID].ParentLocalID]
}

func (e *DirectExecutor) teamID(ctx context.Context) (string, error) {
	if e.Cfg.TeamKey == "" {
		return "", fmt.Errorf("linear.team_id is unset — direct sync has no team to write into")
	}
	var d struct {
		Teams struct{ Nodes []idNode } `json:"teams"`
	}
	err := e.C.Do(ctx, `query($k: String!) { teams(filter: {key: {eq: $k}}) { nodes { id } } }`,
		map[string]any{"k": e.Cfg.TeamKey}, &d)
	if err != nil {
		return "", err
	}
	if len(d.Teams.Nodes) == 0 {
		return "", fmt.Errorf("linear: no team with key %q", e.Cfg.TeamKey)
	}
	return d.Teams.Nodes[0].ID, nil
}

func (e *DirectExecutor) workflowStates(ctx context.Context, teamID string) (map[string]string, error) {
	type st struct{ ID, Name string }
	all, err := linearapi.Paginate(func(after string) ([]st, string, bool, error) {
		var d struct {
			Team struct {
				States struct {
					Nodes    []st `json:"nodes"`
					PageInfo page `json:"pageInfo"`
				} `json:"states"`
			} `json:"team"`
		}
		err := e.C.Do(ctx, `query($t: String!, $a: String) { team(id: $t) { states(first: 100, after: $a) { nodes { id name } pageInfo { hasNextPage endCursor } } } }`,
			map[string]any{"t": teamID, "a": nilIfEmpty(after)}, &d)
		return d.Team.States.Nodes, d.Team.States.PageInfo.End, d.Team.States.PageInfo.HasNext, err
	})
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(all))
	for _, s := range all {
		m[strings.ToLower(s.Name)] = s.ID
	}
	return m, nil
}

func (e *DirectExecutor) verifyIssues(ctx context.Context, recorded []string) error {
	if len(recorded) == 0 {
		return nil
	}
	found := map[string]bool{}
	for start := 0; start < len(recorded); start += 100 {
		end := min(start+100, len(recorded))
		chunk := recorded[start:end]
		nodes, err := linearapi.Paginate(func(after string) ([]idNode, string, bool, error) {
			var d struct {
				Issues struct {
					Nodes    []idNode `json:"nodes"`
					PageInfo page     `json:"pageInfo"`
				} `json:"issues"`
			}
			err := e.C.Do(ctx, `query($ids: [ID!], $a: String) { issues(filter: {id: {in: $ids}}, first: 100, after: $a) { nodes { id } pageInfo { hasNextPage endCursor } } }`,
				map[string]any{"ids": chunk, "a": nilIfEmpty(after)}, &d)
			return d.Issues.Nodes, d.Issues.PageInfo.End, d.Issues.PageInfo.HasNext, err
		})
		if err != nil {
			return err
		}
		for _, n := range nodes {
			found[n.ID] = true
		}
	}
	var missing []string
	for _, id := range recorded {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("linear: recorded id(s) not found in Linear (deleted or wrong workspace?): %s — not recreating", strings.Join(missing, ", "))
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// first returns the first id of a connection query, "" if none.
func (e *DirectExecutor) first(ctx context.Context, query string, vars map[string]any, field string) (string, error) {
	var d map[string]struct{ Nodes []idNode }
	if err := e.C.Do(ctx, query, vars, &d); err != nil {
		return "", err
	}
	if n := d[field].Nodes; len(n) > 0 {
		return n[0].ID, nil
	}
	return "", nil
}

// ensureStructure finds-or-creates one Initiative/Project/Milestone node by
// name under its parent and records its id in ids; reports whether it created.
func (e *DirectExecutor) ensureStructure(ctx context.Context, op Op, byLocal map[string]Node, ids map[string]string, teamID string) (bool, error) {
	title := op.Fields["title"]
	parent := ids[byLocal[op.LocalID].ParentLocalID]
	var id string
	var err error
	switch op.Level {
	case "initiative":
		if e.Cfg.RootInitiative != "" {
			ids[op.LocalID] = e.Cfg.RootInitiative
			return false, nil
		}
		if id, err = e.first(ctx, `query($n: String!) { initiatives(filter: {name: {eq: $n}}) { nodes { id } } }`, map[string]any{"n": title}, "initiatives"); err != nil || id != "" {
			break
		}
		var d struct {
			InitiativeCreate struct{ Initiative idNode } `json:"initiativeCreate"`
		}
		if err = e.C.Do(ctx, `mutation($in: InitiativeCreateInput!) { initiativeCreate(input: $in) { success initiative { id } } }`,
			map[string]any{"in": map[string]any{"name": title}}, &d); err != nil {
			return false, err
		}
		ids[op.LocalID] = d.InitiativeCreate.Initiative.ID
		return true, nil
	case "project":
		if id, err = e.first(ctx, `query($n: String!, $t: ID!) { projects(filter: {name: {eq: $n}, accessibleTeams: {some: {id: {eq: $t}}}}) { nodes { id } } }`, map[string]any{"n": title, "t": teamID}, "projects"); err != nil || id != "" {
			break
		}
		var d struct {
			ProjectCreate struct{ Project idNode } `json:"projectCreate"`
		}
		if err = e.C.Do(ctx, `mutation($in: ProjectCreateInput!) { projectCreate(input: $in) { success project { id } } }`,
			map[string]any{"in": map[string]any{"name": title, "teamIds": []string{teamID}}}, &d); err != nil {
			return false, err
		}
		ids[op.LocalID] = d.ProjectCreate.Project.ID
		if parent != "" {
			if err = e.C.Do(ctx, `mutation($in: InitiativeToProjectCreateInput!) { initiativeToProjectCreate(input: $in) { success } }`,
				map[string]any{"in": map[string]any{"initiativeId": parent, "projectId": ids[op.LocalID]}}, nil); err != nil {
				return true, err
			}
		}
		return true, nil
	case "milestone":
		if parent == "" {
			return false, fmt.Errorf("parent project has no Linear id")
		}
		if id, err = e.first(ctx, `query($n: String!, $p: ID!) { projectMilestones(filter: {name: {eq: $n}, project: {id: {eq: $p}}}) { nodes { id } } }`, map[string]any{"n": title, "p": parent}, "projectMilestones"); err != nil || id != "" {
			break
		}
		var d struct {
			Create struct {
				ProjectMilestone idNode `json:"projectMilestone"`
			} `json:"projectMilestoneCreate"`
		}
		if err = e.C.Do(ctx, `mutation($in: ProjectMilestoneCreateInput!) { projectMilestoneCreate(input: $in) { success projectMilestone { id } } }`,
			map[string]any{"in": map[string]any{"projectId": parent, "name": title}}, &d); err != nil {
			return false, err
		}
		ids[op.LocalID] = d.Create.ProjectMilestone.ID
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// Found by name: already exists. (No close handling: Plan only emits create
	// for Project/Milestone/Epic — they carry no recorded id, so they are never
	// update/close ops.)
	ids[op.LocalID] = id
	return false, nil
}
