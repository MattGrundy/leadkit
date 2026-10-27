package hierarchy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/review"
)

// Subtask is one store task, reduced to what rollup/propose need.
type Subtask struct {
	ID        string
	Status    string
	Parent    string            // "" = no parent: frontmatter field at all
	Milestone string            // frontmatter `milestone:` — a Task's direct milestone tag (queried by the milestone DoD's `tasks` signal)
	Epic      string            // frontmatter `epic:` — a Task's direct epic tag; the `tasks` DoD signal rolls this up to the epic's milestone (a large epic's tasks all tag `epic: <its id>`)
	Type      string            // raw frontmatter `type:`; LoadSubtasksResolved overwrites this with the inferred type
	Owner     string            // frontmatter `owner:` — input to the type -> owner -> lane inference cascade
	Priority  string            // frontmatter `priority:` (P0..P3) — read by the GitHub mirror's label mapping, ignored by rollup/propose
	Title     string            // frontmatter `title` (falling back to `subject`, the pre-2026-09 field name)
	Trackers  map[string]string // parsed `trackers:` flow-map, e.g. {"linear": "ABC-123"} — see internal/sync
}

// LoadSubtasks reads every *.md in storeRoot's frontmatter, best-effort (a file with
// no/malformed frontmatter is skipped, matching every other disk-reading lens in
// this codebase). milestones.yaml itself is never picked up (it doesn't end in
// ".md").
func LoadSubtasks(storeRoot string) ([]Subtask, error) {
	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		return nil, fmt.Errorf("hierarchy: read store root %s: %w", storeRoot, err)
	}
	var out []Subtask
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fm, err := review.ReadFrontmatter(filepath.Join(storeRoot, e.Name()))
		if err != nil {
			continue
		}
		id := strings.TrimSpace(fm["id"])
		if id == "" {
			id = idFromFilename(e.Name())
		}
		if id == "" {
			continue
		}
		title := fm["title"]
		if title == "" {
			title = fm["subject"] // older tasks predate the title: convention
		}
		out = append(out, Subtask{
			ID: id, Status: fm["status"], Parent: strings.TrimSpace(fm["parent"]),
			Milestone: strings.TrimSpace(fm["milestone"]),
			Epic:      strings.TrimSpace(fm["epic"]),
			Type:      strings.TrimSpace(fm["type"]), Priority: strings.TrimSpace(fm["priority"]), Owner: strings.TrimSpace(fm["owner"]), Title: title,
			Trackers: parseTrackers(fm["trackers"]),
		})
	}
	return out, nil
}

// OpenTasksForMilestone counts store tasks (tasks/*.md under storeRoot) that belong to
// milestone-or-epic id THROUGH THE HIERARCHY and whose status is NON-terminal — the
// `tasks` DoD signal the milestone idle-gate reads. Membership is NOT a flat field
// match: a task belongs to id when id is in its rollup ancestry — its direct
// `milestone:`/`epic:`/`parent:` tags, OR any of those climbed up the locked taxonomy
// (Issue -> its epic -> that epic's milestone; epic -> its milestone). So a large epic's tasks
// tagged `epic: 42` (their `milestone:` holds sub-numbers, `parent:` holds Issue IDs)
// count for id 42, and a task tagged only `parent: I78` rolls up through I78's epic to
// its milestone. It REUSES the existing loaders (LoadSubtasks, LoadIssues, Parse over
// <storeRoot>/milestones.yaml) and the same terminalStatuses set — it does NOT fork the
// hierarchy model.
//
// It also reports `known`: whether id names a real node anywhere in the hierarchy — a
// milestone/epic/nested-task in milestones.yaml, an Issue, or a node any task rolls up
// to. This lets the caller distinguish a GENUINE zero (id is a known milestone/epic, all
// its tasks terminal → MET) from a MIS-KEYED id (matches nothing → the caller reads it as
// UNVERIFIED, never a silent false-MET). A id of "" matches nothing (0, not known). A
// missing/unreadable store surfaces the error so callers can degrade rather than report a
// false 0; a missing milestones.yaml/issues dir is not an error (absence is normal).
func OpenTasksForMilestone(storeRoot, issuesRoot, id string) (count int, known bool, err error) {
	if id == "" {
		return 0, false, nil
	}
	subtasks, err := LoadSubtasks(storeRoot)
	if err != nil {
		return 0, false, err
	}
	issues, err := LoadIssues(issuesRoot)
	if err != nil {
		return 0, false, err
	}
	model, _, err := Parse(filepath.Join(storeRoot, "milestones.yaml"))
	if err != nil {
		return 0, false, err
	}

	epicToMilestone := map[string]string{}
	for _, ms := range model.Milestones {
		for _, e := range ms.Epics {
			epicToMilestone[e.ID] = ms.ID
		}
	}
	issueToEpic := map[string]string{}
	for _, is := range issues {
		issueToEpic[is.ID] = is.Parent
	}

	// id is known if it names a real hierarchy node: a milestone/epic/nested-task in
	// milestones.yaml, or an Issue.
	if model.MilestoneIDs()[id] || model.KnownIDs()[id] {
		known = true
	}
	for _, is := range issues {
		if is.ID == id {
			known = true
			break
		}
	}

	for _, s := range subtasks {
		if !taskAncestors(s, issueToEpic, epicToMilestone)[id] {
			continue
		}
		known = true // a task rolls up to id → id is a real, tagged node (not a typo)
		if terminalStatuses[normalizeStatus(s.Status)] {
			continue
		}
		count++
	}
	return count, known, nil
}

// taskAncestors returns the set of milestone/epic/issue ids a task rolls up to: its
// direct `milestone:`/`epic:`/`parent:` tags, plus each of those climbed up the taxonomy
// (Issue -> its epic -> that epic's milestone; epic -> its milestone).
func taskAncestors(s Subtask, issueToEpic, epicToMilestone map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, tag := range []string{s.Milestone, s.Epic, s.Parent} {
		climbHierarchy(tag, out, issueToEpic, epicToMilestone)
	}
	return out
}

// climbHierarchy adds id and every ancestor above it (Issue -> epic -> milestone; epic ->
// milestone) to out. A bare id with no known parents adds just itself.
func climbHierarchy(id string, out map[string]bool, issueToEpic, epicToMilestone map[string]string) {
	if id == "" {
		return
	}
	out[id] = true
	if epic, ok := issueToEpic[id]; ok && epic != "" {
		out[epic] = true
		if ms := epicToMilestone[epic]; ms != "" {
			out[ms] = true
		}
	}
	if ms := epicToMilestone[id]; ms != "" {
		out[ms] = true
	}
}

// idFromFilename extracts a store task's leading "NNN-" id from its filename — used
// only when the frontmatter itself carries no explicit `id:` field. Duplicated
// (rather than imported) across internal/orchestrate, internal/backfill, and here —
// this codebase's established small-package decoupling convention.
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
