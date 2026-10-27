package hook

import (
	"strings"

	"mgpp.dev/leadkit/internal/audit"
)

// task-mechanism has no bash predecessor — it's new: the active, at-creation-time
// analogue of brief-length-guard, catching the exact misuse internal/audit's session
// lens can only report after the fact (56 live at the time this was built). It calls
// audit.IsWorkish DIRECTLY (not a reimplementation) so the guard and the audit can
// never disagree about what "workish" means.
//
// WIRING (not performed here, zero-impact this step): a later, reviewed `leadkit
// init` wires this as a PreToolUse(TaskCreate|TaskUpdate) hook pointing at
// `leadkit hook task-mechanism`. This subcommand does not touch settings.json.

// sessionTaskOKMarker is the explicit, deliberately awkward-to-type override —
// mirrors brief-length-guard's "[LONG-BRIEF:" escape hatch.
const sessionTaskOKMarker = "[SESSION-TASK-OK:"

// TaskMechanism blocks a TaskCreate/TaskUpdate whose task carries real work but isn't
// a gate task and has no override marker. A TaskUpdate that doesn't touch the
// description (a bare status change, say) naturally passes — an empty description is
// never workish — so "only when it adds a substantial description" falls out of the
// same check without special-casing the tool name.
func TaskMechanism(in Input, cfg audit.WorkishConfig) Result {
	if in.ToolName != "TaskCreate" && in.ToolName != "TaskUpdate" {
		return allow
	}

	if isGateTaskInput(in.ToolInput.Metadata) {
		return allow
	}
	if strings.Contains(in.ToolInput.Subject, sessionTaskOKMarker) ||
		strings.Contains(in.ToolInput.Description, sessionTaskOKMarker) {
		return allow
	}
	if !audit.IsWorkish(in.ToolInput.Description, cfg) {
		return allow
	}

	return Result{Stderr: taskMechanismGuidance, ExitCode: 2}
}

// isGateTaskInput mirrors audit.SessionTask.isGate()'s metadata.gate==true check,
// applied to a TaskCreate/TaskUpdate's raw tool_input.metadata rather than a
// persisted SessionTask record — the two are read from different JSON shapes (a
// tool call's input vs. the file it produces) so this stays a small local check
// rather than forcing a shared type across the create-time and after-the-fact paths.
func isGateTaskInput(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}
	b, _ := metadata["gate"].(bool)
	return b
}

const taskMechanismGuidance = `BLOCKED: this session task reads as real work — session tasks are for ephemeral gate coordination
only; the durable backlog lives on disk. Create a disk task under tasks/ and route it via
send-to-review / queue move instead. If this genuinely must be a session task, prefix the subject
with [SESSION-TASK-OK: <reason>].
`
