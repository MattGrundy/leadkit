package hierarchy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/review"
)

// Issue is one issues/*.md file — the taxonomy's Issue level, between Epic
// (milestones.yaml) and Task (tasks/*.md): Epic -> Issue -> Task.
type Issue struct {
	ID       string
	Status   string
	Parent   string // "" = no parent: field at all — unmapped; else expected to be an epic id
	Title    string
	Trackers map[string]string // parsed `trackers:` flow-map, e.g. {"linear": "ABC-123"} — see internal/sync
}

// LoadIssues reads every *.md in issuesRoot's frontmatter, best-effort — a file with
// no/malformed frontmatter is skipped, matching LoadSubtasks and every other
// disk-reading lens in this codebase. A missing issuesRoot (the dir hasn't been
// seeded yet) returns nil, nil — not an error, same "absence is normal" convention
// as Parse's missing-milestones.yaml case.
func LoadIssues(issuesRoot string) ([]Issue, error) {
	entries, err := os.ReadDir(issuesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hierarchy: read issues root %s: %w", issuesRoot, err)
	}
	var out []Issue
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fm, err := review.ReadFrontmatter(filepath.Join(issuesRoot, e.Name()))
		if err != nil {
			continue
		}
		id := strings.TrimSpace(fm["id"])
		if id == "" {
			id = idFromIssueFilename(e.Name())
		}
		if id == "" {
			continue
		}
		out = append(out, Issue{
			ID: id, Status: fm["status"], Parent: strings.TrimSpace(fm["parent"]), Title: fm["title"],
			Trackers: parseTrackers(fm["trackers"]),
		})
	}
	return out, nil
}

// idFromIssueFilename extracts a "I<N>-slug.md" filename's leading id (its own
// id-space — see internal/issuecreate's package doc for why it's prefixed) — used
// only when the frontmatter itself carries no explicit `id:` field. Deliberately
// separate from subtasks.go's idFromFilename (that one expects a bare-integer leading
// id, a Task's shape, not an Issue's).
func idFromIssueFilename(name string) string {
	if !strings.HasPrefix(name, "I") {
		return ""
	}
	i := 1
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 1 || i >= len(name) || name[i] != '-' {
		return ""
	}
	return name[:i]
}
