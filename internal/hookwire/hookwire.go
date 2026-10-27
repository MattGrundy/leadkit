// Package hookwire is the single source of truth for how leadkit's guard hooks wire
// into Claude Code's settings.json — what PreToolUse matcher + command each expects,
// how to tell whether a given settings.json already has one wired, and the JSON
// shapes needed to read (and, in internal/install, write) that file's "hooks" block.
//
// Two callers share this: internal/doctor's read-only hook-wiring check (is a hook
// actually installed?) and internal/install's `leadkit init` (make it so). Both need
// to agree on the exact same mapping — this package IS that mapping, so the two can
// never drift apart.
package hookwire

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// HookSpec is one leadkit guard hook's desired wiring.
type HookSpec struct {
	Name string // the `leadkit hook <name>` subcommand name
	// Event is the settings.json hook event this hook wires under. Every leadkit
	// guard today is PreToolUse; kept as a field (not a hardcoded constant) so a
	// future non-PreToolUse hook doesn't need a structural change here.
	Event string
	// RequiredTools are the tool name(s) the matcher must cover for this hook to
	// count as correctly wired — ALL of them, or a bare "*" matcher. Most hooks need
	// exactly one; task-mechanism needs both TaskCreate and TaskUpdate.
	RequiredTools []string
	// WireMatcher, when non-empty, is the EXACT settings.json matcher string
	// `leadkit init` wires this hook under — the hook's FULL intended matcher set,
	// which can be BROADER than RequiredTools. usage-logger does tool telemetry on
	// Bash AND spawn telemetry on Agent|Task, so it wires under the alternation
	// "Bash|Agent|Task" (init writes one entry that fires on all three tool events).
	// Empty means "wire under joinMatcher(RequiredTools)" — the historical behaviour.
	//
	// RequiredTools stays the MINIMUM a matcher must COVER for a hook to count as
	// wired (doctor's check) — deliberately NOT widened here, so a legacy usage-logger
	// found under a bare "Bash" is still reported wired, not newly flagged. WireMatcher
	// governs only (a) what init WRITES on a fresh wire and (b) the coverage floor
	// init's consolidation must never shrink a hook below (see internal/install's
	// matcher-aware repair — a repair must never drop an intentional second matcher).
	WireMatcher string
}

// InitMatcher is the matcher string `leadkit init` wires this hook under: WireMatcher
// when set, else the RequiredTools joined by "|". This is the canonical matcher init
// adds a fresh entry with, and the intended-token floor its repair never shrinks below.
func (s HookSpec) InitMatcher() string {
	if s.WireMatcher != "" {
		return s.WireMatcher
	}
	return strings.Join(s.RequiredTools, "|")
}

// ExpectedHooks is the canonical mapping every leadkit guard hook wires to. Order is
// the order doctor/init report/apply them in.
var ExpectedHooks = []HookSpec{
	{Name: "brief-length", Event: "PreToolUse", RequiredTools: []string{"SendMessage"}},
	{Name: "pr-guard", Event: "PreToolUse", RequiredTools: []string{"Bash"}},
	{Name: "queue-guard", Event: "PreToolUse", RequiredTools: []string{"Bash"}},
	{Name: "usage-logger", Event: "PreToolUse", RequiredTools: []string{"Bash"}, WireMatcher: "Bash|Agent|Task"},
	{Name: "push-guard", Event: "PreToolUse", RequiredTools: []string{"Bash"}},
	{Name: "push-gate", Event: "PreToolUse", RequiredTools: []string{"Bash"}},
	{Name: "task-mechanism", Event: "PreToolUse", RequiredTools: []string{"TaskCreate", "TaskUpdate"}},
	{Name: "task-status-guard", Event: "PreToolUse", RequiredTools: []string{"Edit", "Write", "MultiEdit"}},
	{Name: "owner-guard", Event: "PreToolUse", RequiredTools: []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "Bash"}},
	{Name: "milestone-backstop", Event: "PreToolUse", RequiredTools: []string{"SendMessage"}},
	// routing-inbox surfaces open account-level routing items on a session's activity
	// (docs/build-tasks/routing-substrate.md). Wired under the same broad activity
	// alternation usage-logger uses, so a live recipient sees a durable hand-off on its
	// next tool call — deduped so it never nags. Available for `leadkit init` to wire;
	// the lead applies init deliberately (init is dry-run by default).
	{Name: "routing-inbox", Event: "PreToolUse", RequiredTools: []string{"Bash"}, WireMatcher: "Bash|Agent|Task"},
}

// KnownBashEquivalent is the pre-existing bash hook script a given leadkit hook
// replaces — a hook absent here is brand new (task-mechanism, push-guard) with no bash
// predecessor to flag as redundant. Matched against a settings.json command string by
// substring (bash invocations vary: `bash $HOME/.claude/hooks/x.sh`,
// `bash '/Users/.../hooks/x.sh'`, a bare absolute path, ...). push-gate is the Go
// replacement for review-gate's own PreToolUse script (not under hooks/), so once it's
// wired + trusted the lead removes the `bash $HOME/.claude/review-gate/pretooluse-hook.sh`
// settings entry — the two otherwise double-guard the same ungated push (harmless, redundant).
var KnownBashEquivalent = map[string]string{
	"brief-length":      "hooks/brief-length-guard.sh",
	"pr-guard":          "hooks/pr-repo-guard.sh",
	"queue-guard":       "hooks/queue-mv-guard.sh",
	"usage-logger":      "hooks/usage-logger.sh",
	"push-gate":         "review-gate/pretooluse-hook.sh",
	"task-status-guard": "hooks/task-status-guard.sh",
	"owner-guard":       "hooks/owner-guard.sh",
}

// HookCommand is one settings.json `hooks[].hooks[]` entry.
type HookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// MatcherGroup is one settings.json `hooks.<Event>[]` entry: a matcher (tool-name
// regex-ish string, e.g. "Bash" or "Edit|Write|MultiEdit") and the commands that fire
// for it.
type MatcherGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
}

// settingsHooksOnly is the minimal shape read out of a settings.json — every other
// top-level key (permissions, statusLine, ...) is deliberately ignored here (doctor
// never writes; internal/install handles preserving those for the actual write path).
type settingsHooksOnly struct {
	Hooks map[string][]MatcherGroup `json:"hooks"`
}

// LoadHooks reads path's "hooks" block. A missing file is NOT an error — returns
// (nil, false, nil), matching both doctor's "no settings found, not a crash" and
// init's "start from {} if absent". A present-but-malformed file IS an error.
func LoadHooks(path string) (hooks map[string][]MatcherGroup, found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("hookwire: read %s: %w", path, err)
	}
	var s settingsHooksOnly
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, true, fmt.Errorf("hookwire: parse %s: %w", path, err)
	}
	return s.Hooks, true, nil
}

// MergeHooks combines several settings files' hook maps (e.g. user + project +
// project-local settings.json) into one, per event — a hook counts as wired if it
// appears in ANY of them. Later maps' groups are appended after earlier ones;
// nothing is deduplicated (EvaluateWiring only needs "does at least one entry
// match", not a canonical merged list).
func MergeHooks(maps ...map[string][]MatcherGroup) map[string][]MatcherGroup {
	out := map[string][]MatcherGroup{}
	for _, m := range maps {
		for event, groups := range m {
			out[event] = append(out[event], groups...)
		}
	}
	return out
}

// matcherCovers reports whether matcher (a settings.json matcher string, e.g. "Bash"
// or "Edit|Write|MultiEdit|NotebookEdit|Bash") covers every tool in required — a bare
// "*" covers everything; otherwise every required tool must appear as one of
// matcher's "|"-separated tokens (exact token match, not substring — "Bash" must not
// be satisfied by e.g. a hypothetical "BashOutput" token).
func matcherCovers(matcher string, required []string) bool {
	if matcher == "*" {
		return true
	}
	tokens := map[string]bool{}
	for _, t := range strings.Split(matcher, "|") {
		tokens[strings.TrimSpace(t)] = true
	}
	for _, r := range required {
		if !tokens[r] {
			return false
		}
	}
	return true
}

// commandPattern builds the "does this command invoke the leadkit `hook <name>`
// subcommand" matcher. Deliberately does NOT require the literal binary name
// "leadkit" before "hook" — a real install may be invoked as a bare `leadkit`, an
// absolute path to that name (`/path/to/leadkit hook push-guard`), OR an absolute path
// to a differently-named binary (`/Users/.../leadkit-bin hook push-guard`, the actual
// deployed name — see ONBOARDING.md's deployment note). Canonicalization is the whole
// point here: a bare invocation and any absolute-path invocation of the SAME hook
// subcommand must be recognized as the SAME wired hook regardless of what precedes
// "hook <name>", so internal/install's idempotency + repair logic (which reuses this
// via Invokes) doesn't add an absolute duplicate next to a bare entry or vice-versa.
// Requires a word boundary before "hook" (so "unhook push-guard" doesn't
// false-positive) and after the hook name (so "push-guard" doesn't match a
// hypothetical "push-guard-v2").
func commandPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[/\s])hook\s+` + regexp.QuoteMeta(name) + `(\s|$)`)
}

// commandInvokes reports whether command invokes the named leadkit hook.
func commandInvokes(command, name string) bool {
	return commandPattern(name).MatchString(command)
}

// Invokes is the exported form of commandInvokes — internal/install's repair logic
// (finding and consolidating every existing entry for a given hook, bare or absolute,
// single or duplicated) reuses this SAME detection rather than forking its own
// pattern, so the two can never drift apart on what counts as "this hook is wired".
func Invokes(command, name string) bool {
	return commandInvokes(command, name)
}

// BinaryToken returns the binary-path token a command uses to invoke `hook <name>` —
// everything BEFORE the "hook <name>" match, whitespace-trimmed (e.g. from
// "/Users/x/.claude/leadkit-bin hook usage-logger" it returns
// "/Users/x/.claude/leadkit-bin"; from a bare "leadkit hook usage-logger", "leadkit").
// Returns "" when command doesn't invoke the hook, or nothing precedes the invocation.
// internal/install uses this to symlink-resolve the leadkit binary token so a
// symlink-equivalent path (e.g. ~/.local/bin/leadkit -> ~/.claude/leadkit-bin) isn't
// churn-repaired as if it were a stale absolute path — reusing this SAME pattern rather
// than re-deriving the split so the two can never disagree on where the binary ends.
func BinaryToken(command, name string) string {
	loc := commandPattern(name).FindStringIndex(command)
	if loc == nil {
		return ""
	}
	return strings.TrimSpace(command[:loc[0]])
}

// WireStatus is one leadkit hook's install-check verdict.
type WireStatus struct {
	Name         string
	Wired        bool   // an entry invoking this hook exists, under a matcher that covers it
	Found        bool   // an entry invoking this hook exists AT ALL (even under a wrong matcher)
	Command      string // the matched command string, if Found
	FoundMatcher string // the matcher the found entry is under, if Found
}

// Misconfigured reports whether this hook was found but under a matcher that doesn't
// cover what it needs — distinct from simply missing.
func (w WireStatus) Misconfigured() bool { return w.Found && !w.Wired }

// EvaluateWiring checks every ExpectedHooks entry against hooks (as returned by
// LoadHooks/MergeHooks) and reports each one's wiring status. Pure and file-free —
// this is what both doctor's check and every test exercise directly.
func EvaluateWiring(hooks map[string][]MatcherGroup) []WireStatus {
	statuses := make([]WireStatus, 0, len(ExpectedHooks))
	for _, spec := range ExpectedHooks {
		st := WireStatus{Name: spec.Name}
		for _, group := range hooks[spec.Event] {
			for _, h := range group.Hooks {
				if !commandInvokes(h.Command, spec.Name) {
					continue
				}
				st.Found = true
				st.Command = h.Command
				st.FoundMatcher = group.Matcher
				if matcherCovers(group.Matcher, spec.RequiredTools) {
					st.Wired = true
				}
			}
		}
		statuses = append(statuses, st)
	}
	return statuses
}
