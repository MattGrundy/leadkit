package milestone

import (
	"fmt"
	"strings"
)

// stateGlyph renders a condition state as a compact ✓/✗/? marker.
func stateGlyph(s State) string {
	switch s {
	case Met:
		return "✓"
	case Unmet:
		return "✗"
	default:
		return "?"
	}
}

// absentNotice is the shared degrade line for an absent artifact — status and check
// both print it, cross-linking init + doctor. It is NOT a false GO.
const absentNotice = "no milestone DoD declared — run `leadkit milestone init` (see `leadkit doctor`)"

// FormatStatus renders the per-tick re-anchor: the current milestone, the open-task
// count, and each DoD condition's met/unmet/unverified state. Degrades cleanly to the
// absent notice when no artifact is declared.
func FormatStatus(rep Report) string {
	var b strings.Builder
	if !rep.Declared {
		fmt.Fprintf(&b, "milestone: %s\n", absentNotice)
		return b.String()
	}
	fmt.Fprintf(&b, "milestone: %s\n", rep.CurrentMilestone)
	if rep.OpenTaskCountKnown {
		fmt.Fprintf(&b, "open milestone tasks: %d\n", rep.OpenTaskCount)
	} else {
		fmt.Fprintf(&b, "open milestone tasks: (not counted — store unreachable)\n")
	}
	if len(rep.Conditions) == 0 {
		b.WriteString("DoD: (no conditions declared)\n")
		return b.String()
	}
	b.WriteString("DoD:\n")
	for _, c := range rep.Conditions {
		fmt.Fprintf(&b, "  %s %-10s %-12s %s — %s\n", stateGlyph(c.State), c.State, "["+c.Type+"]", c.Name, c.Detail)
	}
	return b.String()
}

// FormatCheck renders the idle-gate verdict: a GO line when the milestone is clean, or a
// BLOCK line naming exactly what's open/unverified. mode is the resolved gate mode
// ("block"|"warn") — printed so the reader sees which strictness applied. This is the
// SAME report in both modes; only the exit code differs (the CLI's concern).
func FormatCheck(rep Report, mode string) string {
	var b strings.Builder
	if !rep.Blocked() {
		fmt.Fprintf(&b, "milestone check: GO — milestone %s: all DoD conditions met, 0 open tasks\n", rep.CurrentMilestone)
		return b.String()
	}
	verb := "BLOCK"
	if mode == ModeWarn {
		verb = "WARN"
	}
	if !rep.Declared {
		fmt.Fprintf(&b, "milestone check: %s — %s\n", verb, absentNotice)
		return b.String()
	}
	fmt.Fprintf(&b, "milestone check: %s (mode=%s) — milestone %s is not at a rest point:\n", verb, mode, rep.CurrentMilestone)
	for _, line := range rep.Blockers() {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	return b.String()
}

// Gate mode vocabulary.
const (
	ModeBlock = "block"
	ModeWarn  = "warn"
)

// ResolveMode applies the resolution order: an explicit flag > the profile's
// milestone.idle_gate_mode > the default (block). An unrecognised value at any level is
// treated as unset (falls through) rather than erroring — the gate must never be
// silently disabled by a typo, so it defaults to the stricter block.
func ResolveMode(flag, profileMode string) string {
	if m := normalizeMode(flag); m != "" {
		return m
	}
	if m := normalizeMode(profileMode); m != "" {
		return m
	}
	return ModeBlock
}

func normalizeMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ModeBlock:
		return ModeBlock
	case ModeWarn:
		return ModeWarn
	default:
		return ""
	}
}
