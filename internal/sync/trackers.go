package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/review"
)

// parseTrackers parses frontmatter's `trackers:` line value — a small inline
// flow-map like "{}" or "{linear: ABC-123, jira: XYZ-1}" — into a plain map.
// Duplicated from internal/hierarchy's own copy (this codebase's small-package
// decoupling convention) — sync is the write side (RecordID needs to merge into
// an existing trackers value), hierarchy is the read side.
func parseTrackers(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// formatTrackers is parseTrackers' inverse — a deterministic (sorted-key) inline
// flow-map string, e.g. "{jira: XYZ-1, linear: ABC-123}".
func formatTrackers(trackers map[string]string) string {
	if len(trackers) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(trackers))
	for k := range trackers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %s", k, trackers[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// RecordID writes trackers[output] = trackerID into localID's frontmatter — an
// Issue or Task only. Project/Milestone/Epic ids live in milestones.yaml, which
// this project's tooling never auto-writes (the same "never touch
// milestones.yaml" rule internal/hierarchyapply follows) — recording a tracker id
// for those three levels is out of scope here. Idempotent: recording the same id
// again just rewrites the same value.
func RecordID(storeRoot, issuesRoot, output, localID, trackerID string) error {
	path := taskOrIssuePath(storeRoot, issuesRoot, localID)
	if path == "" {
		return fmt.Errorf("sync: no Issue/Task file found for local id %q (Project/Milestone/Epic ids live in milestones.yaml, which record-id never writes)", localID)
	}
	fm, err := review.ReadFrontmatter(path)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	trackers := parseTrackers(fm["trackers"])
	trackers[output] = trackerID
	return review.UpdateFrontmatter(path, []review.KV{{Key: "trackers", Value: formatTrackers(trackers)}})
}

// taskOrIssuePath resolves a local id to its store file: issues/ for an Issue id
// (the "I" prefix — see internal/issuecreate's own doc comment), tasks/
// otherwise.
func taskOrIssuePath(storeRoot, issuesRoot, id string) string {
	root := storeRoot
	if strings.HasPrefix(id, "I") {
		root = issuesRoot
	}
	matches, _ := filepath.Glob(filepath.Join(root, id+"-*.md"))
	if len(matches) > 0 {
		return matches[0]
	}
	p := filepath.Join(root, id+".md")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}
