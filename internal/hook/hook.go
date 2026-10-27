// Package hook folds leadkit's PreToolUse guard hooks into the binary, so hook config
// can point at `leadkit hook <name>` (on PATH, portable) instead of a machine-local
// `~/.claude/hooks/*.sh` absolute path. See docs/PORTABLE-OPERATING-KIT.md.
//
// This step only builds the subcommands: it installs nothing and does not touch
// settings.json — that's kit `init`, later and reviewed.
//
// FAIL OPEN is load-bearing here, exactly as in every source bash hook: on any
// parse/runtime error, or a tool/command a hook doesn't govern, the result is a
// silent allow (exit 0, nothing printed). A broken guard must never block real work.
package hook

import (
	"encoding/json"

	"mgpp.dev/leadkit/internal/audit"
	"mgpp.dev/leadkit/internal/config"
)

// The hooks Dispatch recognizes.
const (
	NameBriefLength   = "brief-length"
	NamePRGuard       = "pr-guard"
	NameQueueGuard    = "queue-guard"
	NameUsageLogger   = "usage-logger"
	NameTaskMechanism = "task-mechanism"
	NamePushGuard     = "push-guard"
	NamePushGate      = "push-gate"
	NameTaskStatus    = "task-status-guard"
	NameOwnerGuard    = "owner-guard"
	NameMilestone     = "milestone-backstop"
	NameRoutingInbox  = "routing-inbox"
)

// Names lists every recognized hook name, for `leadkit hook -h`.
var Names = []string{NameBriefLength, NamePRGuard, NameQueueGuard, NameUsageLogger, NameTaskMechanism, NamePushGuard, NamePushGate, NameTaskStatus, NameOwnerGuard, NameMilestone, NameRoutingInbox}

// toolInput is the subset of tool_input the hooks read between them: `message`
// (SendMessage, for brief-length), `command` (Bash, for pr-guard/queue-guard/
// usage-logger/owner-guard), `subject`/`description`/`metadata` (TaskCreate/TaskUpdate,
// for task-mechanism), or `file_path`/`new_string`/`content`/`edits` (Edit/Write/
// MultiEdit/NotebookEdit, for task-status-guard/owner-guard). A tool that doesn't carry
// the field a given hook needs just leaves it at its zero value.
type toolInput struct {
	Message     string                 `json:"message"`
	Command     string                 `json:"command"`
	Subject     string                 `json:"subject"`
	Description string                 `json:"description"`
	Metadata    map[string]interface{} `json:"metadata"`
	FilePath    string                 `json:"file_path"`
	NewString   string                 `json:"new_string"`
	Content     string                 `json:"content"`
	Edits       []editEntry            `json:"edits"`
}

// editEntry is one element of a MultiEdit's tool_input.edits array — only new_string
// matters to the guards (the written text being introduced).
type editEntry struct {
	NewString string `json:"new_string"`
}

// Input is the PreToolUse hook stdin payload every hook parses:
// { "tool_name": ..., "tool_input": {...}, "session_id": ..., "cwd": ... }.
//
// Cwd is Claude Code's own top-level payload field: the working directory the tool call
// runs in (the session/project dir). cmd/leadkit threads it into the shared target-path
// resolver as the base for absolutizing a RELATIVE `cd`/`-C` push target and as the
// fallback target for a truly-bare `git push` — so a bare push resolves from where the
// tool call actually runs, NOT from os.Getwd() of the hook subprocess. It is empty on an
// older client that omits it (the resolver then falls back to os.Getwd(), no regression).
// NOTE: the payload cwd reflects the SESSION/tool directory; it does not track a
// mid-session `cd` done in a separate prior tool call, so a truly-bare push after such a
// `cd` remains best-effort — the in-command `cd <path> && git push` form (the ambient-profile bug's
// actual case) is resolved from the extracted path itself, independent of this field.
type Input struct {
	ToolName  string    `json:"tool_name"`
	ToolInput toolInput `json:"tool_input"`
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
}

// Result is one hook invocation's outcome — what to print where, and the process
// exit code. Kept separate from the process itself so the decision logic is directly
// testable without capturing os.Stdout/os.Stderr.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// allow is the FAIL-OPEN result: silent, exit 0. Returned for a parse error, an
// unrecognized hook name, an ungoverned tool, or a command a hook doesn't match.
var allow = Result{ExitCode: 0}

// Config bundles every hook's own profile-derived inputs, so Dispatch's signature
// stays stable as hooks are added — each hook reads only the field(s) it needs.
// Every hook but pr-guard gets plain data (no *config.Profile), so this package's
// dependency on internal/config stays confined to the one hook (PRGuard) whose
// own authority schema is non-trivial enough that hand-rolling a parallel struct
// would just be duplication — see pr_guard.go's own doc comment.
type Config struct {
	QueueBase      string                  // queue-guard: the queue root's basename
	LogPath        string                  // usage-logger: the profile's resolved telemetry_log
	Workish        audit.WorkishConfig     // task-mechanism (+ the audit session lens): shared "carries real work" tunables
	PushGuard      PushGuardConfig         // push-guard: the whitelist inputs
	PushGate       PushGateConfig          // push-gate: the review-gate check inputs (per-target gate resolver + cwd/home)
	BriefLengthMax int                     // brief-length: the profile's brief_length_max (0 = use the built-in default)
	Profile        *config.Profile         // pr-guard: the resolved profile (authority.<repo>)
	Cwd            string                  // pr-guard: the invocation's own cwd (used absent a `cd <path> &&` prefix)
	TaskStatus     TaskStatusConfig        // task-status-guard: per-target store_root scope + vocabulary resolver
	OwnerGuard     OwnerGuardConfig        // owner-guard: per-target owners-policy resolver + caller/bypass/paths
	Milestone      MilestoneBackstopConfig // milestone-backstop: the "would milestone check block?" seam
	RoutingInbox   RoutingInboxConfig      // routing-inbox: the "any open routing items to surface?" seam
}

// Dispatch parses stdin's JSON and runs the named hook, failing open on any parse
// error or an unrecognized name.
func Dispatch(name string, stdin []byte, cfg Config) Result {
	var in Input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return allow
	}
	switch name {
	case NameBriefLength:
		return BriefLength(in, cfg.BriefLengthMax)
	case NamePRGuard:
		return PRGuard(in, cfg.Profile, cfg.Cwd)
	case NameQueueGuard:
		return QueueGuard(in, cfg.QueueBase)
	case NameUsageLogger:
		return UsageLogger(in, cfg.LogPath)
	case NameTaskMechanism:
		return TaskMechanism(in, cfg.Workish)
	case NamePushGuard:
		return PushGuard(in, cfg.PushGuard)
	case NamePushGate:
		return PushGate(in, cfg.PushGate)
	case NameTaskStatus:
		return TaskStatusGuard(in, cfg.TaskStatus)
	case NameOwnerGuard:
		return OwnerGuard(in, cfg.OwnerGuard)
	case NameMilestone:
		return MilestoneBackstop(in, cfg.Milestone)
	case NameRoutingInbox:
		return RoutingInbox(in, cfg.RoutingInbox)
	default:
		return allow
	}
}
