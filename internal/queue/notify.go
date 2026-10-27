package queue

import (
	"fmt"
	"os/exec"
	"strings"
)

// The active-notifier PUSH (docs/build-tasks/self-surfacing-diagnostics.md, Part 3) is
// the proactive, OPT-IN surfacing: when a sweep's stalls+broken-links cross a
// threshold, it execs the operator's own notifier so a lead-less adopter is told
// without having to check in. It is NEVER a hard dependency — an empty command, or the
// toggle off, execs nothing; a failed exec is logged, never fatal (mirrors the waker's
// wake_command). Same `{…}`-placeholder shape as push-wait's wake_command, reusing the
// same field-split substitution.

// notifyExec runs the substituted notifier argv — injectable so tests assert on the
// call (and its placeholder substitution) without ever spawning a real process, the
// same seam push-guard's logViolation uses.
var notifyExec = func(args []string) error {
	if len(args) == 0 {
		return nil
	}
	return exec.Command(args[0], args[1:]...).Run()
}

// NotifyThresholdCrossed reports whether stalls+broken meet/exceed threshold — the
// gate on whether the notifier fires. A threshold <= 0 is normalized to 1 (any finding
// fires), so a misconfigured 0 never means "notify on a perfectly clean sweep".
func NotifyThresholdCrossed(s SweepStatus, threshold int) bool {
	if threshold <= 0 {
		threshold = 1
	}
	return s.Stalled+s.Broken+s.Stray >= threshold
}

// Notify execs notifyCommand with the sweep's findings substituted in, but ONLY when
// the command is non-empty AND the threshold is crossed. fired reports whether the exec
// was actually attempted. Placeholders: {stalled} {broken} {total} {msg}. An empty
// command (the default, surface_notify off, or notify_command unset) returns
// fired=false with no error — the notifier is opt-in and never required.
func Notify(notifyCommand string, threshold int, s SweepStatus) (fired bool, err error) {
	if strings.TrimSpace(notifyCommand) == "" {
		return false, nil
	}
	if !NotifyThresholdCrossed(s, threshold) {
		return false, nil
	}
	args := substituteNotifyArgs(notifyCommand, s)
	if len(args) == 0 {
		return false, nil
	}
	return true, notifyExec(args)
}

// substituteNotifyArgs splits notifyCommand into fields and substitutes each
// placeholder — {msg} (which contains spaces) stays a single argument, exactly like
// substituteWakeArgs, so "notify-send {msg}" passes the whole message as one arg.
func substituteNotifyArgs(notifyCommand string, s SweepStatus) []string {
	msg := NotifyMessage(s)
	repl := func(f string) string {
		f = strings.ReplaceAll(f, "{stalled}", fmt.Sprintf("%d", s.Stalled))
		f = strings.ReplaceAll(f, "{broken}", fmt.Sprintf("%d", s.Broken))
		f = strings.ReplaceAll(f, "{total}", fmt.Sprintf("%d", s.Stalled+s.Broken+s.Stray))
		f = strings.ReplaceAll(f, "{msg}", msg)
		return f
	}
	fields := strings.Fields(notifyCommand)
	args := make([]string, 0, len(fields))
	for _, f := range fields {
		args = append(args, repl(f))
	}
	return args
}

// NotifyMessage is the human {msg} placeholder value — a one-line summary of the
// findings, naming the affected lanes so the notification is actionable on its own.
func NotifyMessage(s SweepStatus) string {
	var parts []string
	if s.Stalled > 0 {
		parts = append(parts, fmt.Sprintf("%d stalled lane(s): %s", s.Stalled, strings.Join(s.stalledLaneNames(), ", ")))
	}
	if s.Stray > 0 {
		parts = append(parts, fmt.Sprintf("%d ticket(s) stranded in a non-canonical state dir", s.Stray))
	}
	if s.Broken > 0 {
		parts = append(parts, fmt.Sprintf("%d broken link(s): %s", s.Broken, strings.Join(s.brokenLaneNames(), ", ")))
	}
	if len(parts) == 0 {
		return "leadkit queue sweep: no findings"
	}
	return "leadkit queue sweep: " + strings.Join(parts, "; ")
}
