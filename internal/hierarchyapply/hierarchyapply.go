// Package hierarchyapply is `leadkit hierarchy apply` — materializes a REVIEWED
// mapping plan (a human-edited cluster-issues --json output): creates issues/
// files (via internal/issuecreate) and sets each Task's frontmatter `parent:
// <issue-id>`. Unlike internal/hierarchy's propose/cluster-issues (read-only, see
// that package's own doc comment), this package WRITES — Compute is the pure
// planning half (never touches disk), Apply is the only thing that writes, and
// only when dryRun is false.
//
// SAFETY (incident history — see cmd/leadkit's confirmGuard): dry-run is the
// default; a bare --apply refuses; only --apply --confirm actually writes. Every
// test/dogfood run MUST target a temp store, never the real one — the live apply
// is a coordinated step run separately, same doctrine as taskcreate/issuecreate's
// own safety notes.
//
// Changes ONLY the Task frontmatter `parent` field — never any other field, never
// milestones.yaml, never deletes/moves a Task file.
package hierarchyapply

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/hierarchy"
	"mgpp.dev/leadkit/internal/issuecreate"
	"mgpp.dev/leadkit/internal/recordlog"
	"mgpp.dev/leadkit/internal/review"
)

// PlanEntry is one human-reviewed candidate Issue from a mapping plan —
// cluster-issues' own --json "detail" shape, hand-edited (renamed/split/merged)
// before apply. ParsePlan ignores any other field cluster-issues' raw output
// carries (suggested_name, count, ...) — only these three matter here.
type PlanEntry struct {
	IssueTitle string   `json:"issue_title"`
	Epic       string   `json:"epic"`
	TaskIDs    []string `json:"task_ids"`
}

// ParsePlan accepts either cluster-issues' own wrapped --json shape
// (`{"detail": [...]}`, from `--epic X --json` piped straight into a file) or a
// bare array of PlanEntry (the shape after a human strips the wrapper) — so
// whichever form the plan file is in, it just works.
func ParsePlan(data []byte) ([]PlanEntry, error) {
	var wrapped struct {
		Detail []PlanEntry `json:"detail"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Detail) > 0 {
		return wrapped.Detail, nil
	}
	var bare []PlanEntry
	if err := json.Unmarshal(data, &bare); err != nil {
		return nil, fmt.Errorf("hierarchyapply: parse plan: %w", err)
	}
	return bare, nil
}

// TaskAction is one Task's outcome within a planned Issue.
type TaskAction struct {
	TaskID string `json:"task_id"`
	Action string `json:"action"` // "set" | "already_set" | "skip_conflict" | "not_found"
	Reason string `json:"reason,omitempty"`
}

// IssueAction is what apply would do (or did) for one candidate Issue in the plan.
type IssueAction struct {
	Epic       string       `json:"epic"`
	IssueTitle string       `json:"issue_title"`
	IssueID    string       `json:"issue_id"`
	Reused     bool         `json:"reused"` // true: an existing issue (same title+epic) was found, not created
	Status     string       `json:"status"` // derived: "completed" if every member Task is terminal, else "pending"
	Tasks      []TaskAction `json:"tasks"`
}

// Plan is Compute's full output — pure, read-only; Apply is what executes it.
type Plan struct {
	Issues []IssueAction `json:"issues"`
}

// terminalStatuses/normalizeStatus mirror internal/hierarchy's own copies —
// duplicated on purpose, this codebase's small-package decoupling convention.
var terminalStatuses = map[string]bool{
	"completed": true, "done": true, "dropped": true, "superseded": true,
}

func normalizeStatus(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// Compute validates entries against the real model + store and decides what
// applying them WOULD do — it never writes anything, so it's safe to call for a
// dry-run preview or as Apply's own first half. epicFilter, if set, restricts to
// that epic's entries only. Fails closed: an entry naming an epic that doesn't
// exist in milestones.yaml is a hard error for the WHOLE call — a plan is
// human-edited, so a typo here must never half-apply the rest.
func Compute(entries []PlanEntry, epicFilter, storeRoot, issuesRoot string) (Plan, error) {
	model, _, err := hierarchy.Parse(filepath.Join(storeRoot, "milestones.yaml"))
	if err != nil {
		return Plan{}, fmt.Errorf("hierarchyapply: parse milestones.yaml: %w", err)
	}
	epicIDs := model.EpicIDs()

	for _, e := range entries {
		if epicFilter != "" && e.Epic != epicFilter {
			continue
		}
		if !epicIDs[e.Epic] {
			return Plan{}, fmt.Errorf("hierarchyapply: plan entry %q names unknown epic %q", e.IssueTitle, e.Epic)
		}
	}

	existingIssues, err := hierarchy.LoadIssues(issuesRoot)
	if err != nil {
		return Plan{}, fmt.Errorf("hierarchyapply: load issues: %w", err)
	}
	existingByKey := make(map[string]hierarchy.Issue, len(existingIssues))
	for _, is := range existingIssues {
		existingByKey[issueKey(is.Title, is.Parent)] = is
	}

	subtasks, err := hierarchy.LoadSubtasks(storeRoot)
	if err != nil {
		return Plan{}, fmt.Errorf("hierarchyapply: load subtasks: %w", err)
	}
	subtaskByID := make(map[string]hierarchy.Subtask, len(subtasks))
	for _, s := range subtasks {
		subtaskByID[s.ID] = s
	}

	nextID := issuecreate.NextIDNumber(issuesRoot)
	var plan Plan
	for _, entry := range entries {
		if epicFilter != "" && entry.Epic != epicFilter {
			continue
		}

		key := issueKey(entry.IssueTitle, entry.Epic)
		var issueID string
		reused := false
		if existing, ok := existingByKey[key]; ok {
			issueID, reused = existing.ID, true
		} else {
			issueID = "I" + strconv.Itoa(nextID)
			nextID++
		}

		var tasks []TaskAction
		allTerminal, memberCount := true, 0
		for _, tid := range entry.TaskIDs {
			s, ok := subtaskByID[tid]
			if !ok {
				tasks = append(tasks, TaskAction{TaskID: tid, Action: "not_found", Reason: "no such task in store"})
				continue
			}
			switch s.Parent {
			case issueID:
				tasks = append(tasks, TaskAction{TaskID: tid, Action: "already_set"})
				memberCount++
				if !terminalStatuses[normalizeStatus(s.Status)] {
					allTerminal = false
				}
			case "":
				tasks = append(tasks, TaskAction{TaskID: tid, Action: "set"})
				memberCount++
				if !terminalStatuses[normalizeStatus(s.Status)] {
					allTerminal = false
				}
			default:
				tasks = append(tasks, TaskAction{TaskID: tid, Action: "skip_conflict", Reason: fmt.Sprintf("already parented to %s", s.Parent)})
			}
		}
		status := "pending"
		if memberCount > 0 && allTerminal {
			status = "completed"
		}

		plan.Issues = append(plan.Issues, IssueAction{
			Epic: entry.Epic, IssueTitle: entry.IssueTitle, IssueID: issueID, Reused: reused,
			Status: status, Tasks: tasks,
		})
	}
	return plan, nil
}

func issueKey(title, epic string) string {
	return strings.ToLower(strings.TrimSpace(title)) + "|" + epic
}

// logEntry is one hierarchy-apply.jsonl line.
type logEntry struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"` // "create_issue" | "reuse_issue" | "set_parent"
	IssueID string `json:"issue_id,omitempty"`
	Epic    string `json:"epic,omitempty"`
	Title   string `json:"title,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
}

// Apply executes plan (from Compute): creates any not-yet-existing Issue file
// (deriving its status from plan, never re-deriving from disk again) and sets
// `parent:` on every Task action marked "set". dryRun writes nothing at all —
// Apply still returns the count of actions it WOULD take, so a dry-run and a real
// run report the identical shape. Every actual write is logged to logPath.
func Apply(plan Plan, storeRoot, issuesRoot, logPath string, dryRun bool) (int, error) {
	written := 0
	now := time.Now().UTC().Format(time.RFC3339)

	for _, ia := range plan.Issues {
		if !ia.Reused {
			if !dryRun {
				res, err := issuecreate.Create(issuecreate.Options{
					Title: ia.IssueTitle, Epic: ia.Epic, Status: ia.Status,
					IssuesRoot: issuesRoot, StoreRoot: storeRoot,
				})
				if err != nil {
					return written, fmt.Errorf("hierarchyapply: create issue %q: %w", ia.IssueTitle, err)
				}
				if res.ID != ia.IssueID {
					// Disk moved between Compute and Apply (a concurrent writer claimed
					// ids in between) — refuse rather than silently mis-tag Tasks under
					// the WRONG issue id.
					return written, fmt.Errorf("hierarchyapply: issue id drifted for %q (planned %s, actually claimed %s) — re-run against a fresh plan", ia.IssueTitle, ia.IssueID, res.ID)
				}
				if err := recordlog.Append(logPath, logEntry{TS: now, Kind: "create_issue", IssueID: ia.IssueID, Epic: ia.Epic, Title: ia.IssueTitle}); err != nil {
					return written, fmt.Errorf("hierarchyapply: change log: %w", err)
				}
			}
			written++
		}

		for _, ta := range ia.Tasks {
			if ta.Action != "set" {
				continue
			}
			if !dryRun {
				path := taskPath(storeRoot, ta.TaskID)
				if path == "" {
					return written, fmt.Errorf("hierarchyapply: task %s vanished between plan and apply", ta.TaskID)
				}
				if err := review.UpdateFrontmatter(path, []review.KV{{Key: "parent", Value: ia.IssueID}}); err != nil {
					return written, fmt.Errorf("hierarchyapply: set parent on task %s: %w", ta.TaskID, err)
				}
				if err := recordlog.Append(logPath, logEntry{TS: now, Kind: "set_parent", TaskID: ta.TaskID, IssueID: ia.IssueID}); err != nil {
					return written, fmt.Errorf("hierarchyapply: change log: %w", err)
				}
			}
			written++
		}
	}
	return written, nil
}

// taskPath resolves id to its store file path ("<id>-*.md" or "<id>.md"), or ""
// if no such file exists.
func taskPath(storeRoot, id string) string {
	matches, _ := filepath.Glob(filepath.Join(storeRoot, id+"-*.md"))
	if len(matches) > 0 {
		return matches[0]
	}
	p := filepath.Join(storeRoot, id+".md")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// FormatPlan renders plan for `hierarchy apply`'s output — per-issue create/reuse
// + per-task action, then a one-line summary. dryRun only changes the verb tense.
func FormatPlan(plan Plan, dryRun bool) string {
	var b strings.Builder
	verb := "would create"
	if !dryRun {
		verb = "created"
	}
	setVerb := "would set"
	if !dryRun {
		setVerb = "set"
	}

	issuesWritten, tasksSet, tasksSkipped := 0, 0, 0
	for _, ia := range plan.Issues {
		if ia.Reused {
			fmt.Fprintf(&b, "%s: reused existing issue (epic=%s, status=%s)\n", ia.IssueID, ia.Epic, ia.Status)
		} else {
			fmt.Fprintf(&b, "%s: %s %q (epic=%s, status=%s)\n", ia.IssueID, verb, ia.IssueTitle, ia.Epic, ia.Status)
			issuesWritten++
		}
		for _, ta := range ia.Tasks {
			switch ta.Action {
			case "set":
				fmt.Fprintf(&b, "  %s: %s parent=%s\n", ta.TaskID, setVerb, ia.IssueID)
				tasksSet++
			case "already_set":
				fmt.Fprintf(&b, "  %s: already parent=%s (no-op)\n", ta.TaskID, ia.IssueID)
			case "skip_conflict":
				fmt.Fprintf(&b, "  %s: SKIPPED — %s\n", ta.TaskID, ta.Reason)
				tasksSkipped++
			case "not_found":
				fmt.Fprintf(&b, "  %s: SKIPPED — %s\n", ta.TaskID, ta.Reason)
				tasksSkipped++
			}
		}
	}
	fmt.Fprintf(&b, "\nsummary: %d issue(s) %s, %d task parent(s) %s, %d task(s) skipped\n",
		issuesWritten, verb, tasksSet, setVerb, tasksSkipped)
	return b.String()
}
