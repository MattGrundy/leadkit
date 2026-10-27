package hook

import (
	"fmt"
	"strings"
)

// milestone-backstop is the SOFT, WARN-ONLY PreToolUse(SendMessage) backstop for the
// milestone idle-gate (docs/build-tasks/milestone-anchor-idle-gate.md, Layer 2). It is
// the secondary catch behind the structured `leadkit milestone check`: when a lead→user
// message reads as a claim that the WORK is at rest — the milestone/team is done, at a
// rest point, "nothing needs you" — AND `milestone check` would BLOCK, it injects a
// WARNING. It NEVER hard-blocks a user message (DECISION 2, RESOLVED): prose detection
// is fuzzy, and a false hard-block on a user message is worse than a warn — so this is
// warn-only regardless of the Layer-2 mode.
//
// FOUNDING PRINCIPLE it enforces: the LEAD can be idle; the TEAM cannot. A lead
// announcing its OWN pause WITH work in flight ("I'll idle awaiting the builder/gate/
// CI") is legitimate and passes SILENTLY even when the milestone would block — that is
// the healthy steady state, not a false claim. The backstop keys on WORK-is-at-rest
// phrasing, not on the lead reporting that it personally is pausing.
//
// FAILS OPEN like every guard: not a SendMessage, no evaluator wired, an empty/non-idle
// message, or a would-NOT-block milestone → silent allow.

// MilestoneBackstopConfig carries the backstop's evaluation seam. Evaluate reports
// whether `leadkit milestone check` would BLOCK right now, plus a short summary naming
// what's blocking (milestone id + open-task/unverified-signal counts) for the warning.
// nil Evaluate = disabled (fail open — the hook never warns), so a project with no
// milestone artifact wired is simply unaffected.
type MilestoneBackstopConfig struct {
	Evaluate func() (blocked bool, summary string)
}

// workAtRestPhrases are the lowercased substrings that read as a claim the WORK/TEAM/
// MILESTONE is done or at a rest point — the false-claim moment the gate exists to
// catch. Deliberately NOT "idle" alone: a lead idling is fine, and "I will idle" must
// not trip this.
var workAtRestPhrases = []string{
	"milestone complete", "milestone is complete", "milestone done", "milestone is done",
	"rest point", "at rest",
	"nothing needs you", "nothing needs your attention", "nothing that needs you",
	"nothing will progress", "nothing more to do", "nothing left to do", "nothing further to do",
	"team is idle", "team idle", "team is done", "team done", "team is at rest",
	"we're done", "we are done", "all done", "everything is done",
	"work is done", "work is complete", "all work is done",
}

// leadPauseInFlightPhrases are the lowercased substrings that mark a lead announcing its
// OWN pause WITH work in flight — the legitimate steady state. Their presence makes a
// message pass SILENTLY even if a work-at-rest phrase also appears and the milestone
// would block (the message is about the lead resting while delegated work runs, not a
// claim the work itself is finished).
var leadPauseInFlightPhrases = []string{
	"awaiting", "await ", "waiting on", "waiting for",
	"in flight", "in-flight", "still running", "still building", "still in progress",
	"work in flight",
}

// isWorkAtRestClaim reports whether msg reads as a WORK-is-at-rest claim that is NOT a
// lead-pause-with-work-in-flight announcement.
func isWorkAtRestClaim(msg string) bool {
	m := strings.ToLower(msg)
	hasRest := false
	for _, p := range workAtRestPhrases {
		if strings.Contains(m, p) {
			hasRest = true
			break
		}
	}
	if !hasRest {
		return false
	}
	for _, ex := range leadPauseInFlightPhrases {
		if strings.Contains(m, ex) {
			return false
		}
	}
	return true
}

// MilestoneBackstop is the warn-only SendMessage backstop. Returns a WARNING (stderr,
// exit 0 — NEVER a hard block) when the message reads as a work-at-rest claim and the
// milestone would block; silent allow otherwise.
func MilestoneBackstop(in Input, cfg MilestoneBackstopConfig) Result {
	if in.ToolName != "SendMessage" {
		return allow
	}
	if cfg.Evaluate == nil {
		return allow
	}
	msg := in.ToolInput.Message
	if msg == "" {
		return allow
	}
	if !isWorkAtRestClaim(msg) {
		return allow
	}
	blocked, summary := cfg.Evaluate()
	if !blocked {
		return allow
	}
	return Result{Stderr: fmt.Sprintf(milestoneBackstopWarning, summary), ExitCode: 0}
}

const milestoneBackstopWarning = `WARNING (not a block): this message reads as a claim that the work is at rest, but the milestone is not done.

%s

The lead may idle; the TEAM may not. If you are pausing WITH work in flight (awaiting a
builder/gate/CI), say so explicitly. Otherwise run:

  leadkit milestone check

and resolve the open tasks / unverified signals it names before declaring a rest/done point.
`
