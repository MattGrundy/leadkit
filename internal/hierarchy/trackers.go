package hierarchy

import "strings"

// parseTrackers parses frontmatter's `trackers:` line value — a small inline
// flow-map like "{}" or "{linear: ABC-123, jira: XYZ-1}" — into a plain map. Kept
// deliberately simple (no full YAML parse), matching review.ReadFrontmatter's own
// "flat key: value" parsing philosophy; trackers only ever holds a handful of
// short scalar ids. internal/sync (a separate, write-side package) keeps its own
// duplicate of this — this codebase's established small-package decoupling
// convention (see e.g. terminalStatuses/normalizeStatus, duplicated the same way
// across orchestrate/backfill/hierarchy/hierarchyapply).
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
