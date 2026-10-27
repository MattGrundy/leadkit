package audit

import (
	"fmt"
	"sort"
	"strings"
)

// FormatReport renders findings (+ sessionCounts, if the session lens ran) as the
// human report `audit` prints: grouped by category, then per-session counts, then a
// one-line summary.
func FormatReport(findings []Finding, sessionCounts map[string]SessionCounts) string {
	byCategory := map[string]int{}
	for _, f := range findings {
		byCategory[f.Category]++
	}

	var b strings.Builder
	for _, cat := range Categories {
		var group []Finding
		for _, f := range findings {
			if f.Category == cat {
				group = append(group, f)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d):\n", cat, len(group))
		for _, f := range group {
			fmt.Fprintf(&b, "  %s\n", f)
		}
		b.WriteString("\n")
	}

	if len(sessionCounts) > 0 {
		b.WriteString("session counts:\n")
		var names []string
		for n := range sessionCounts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := sessionCounts[n]
			fmt.Fprintf(&b, "  %s: total=%d gate=%d misuse=%d\n", n, c.Total, c.Gate, c.Misuse)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "audit: %d findings — %d orphaned-active, %d dangling, %d drift, %d dup-id, %d noncanonical, %d session-misuse\n",
		len(findings),
		byCategory[CategoryOrphanedActive], byCategory[CategoryDanglingSymlink], byCategory[CategoryStatusDrift],
		byCategory[CategoryDuplicateID], byCategory[CategoryNoncanonicalStatus], byCategory[CategorySessionMisuse],
	)
	return b.String()
}
