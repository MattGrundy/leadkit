package sync

import (
	"fmt"
	"strings"
)

// FormatOps renders ops as `sync plan`'s human-readable report — one line per Op,
// in the SAME parent-first order Plan produced them (an agent executing them in
// this order never creates a child before its parent).
func FormatOps(ops []Op) string {
	var b strings.Builder
	counts := map[string]int{}
	for _, op := range ops {
		fmt.Fprintf(&b, "%-6s %-10s %-8s", op.Action, op.Level, op.LocalID)
		if op.ParentTrackerID != "" {
			fmt.Fprintf(&b, " parent=%s", op.ParentTrackerID)
		}
		if t, ok := op.Fields["title"]; ok && t != "" {
			fmt.Fprintf(&b, " %q", t)
		}
		b.WriteByte('\n')
		counts[op.Action]++
	}
	fmt.Fprintf(&b, "\n%d op(s): %d create, %d update, %d close\n", len(ops), counts["create"], counts["update"], counts["close"])
	return b.String()
}
